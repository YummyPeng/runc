// +build linux

package libcontainer

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/opencontainers/runc/libcontainer/cgroups"
	"github.com/opencontainers/runc/libcontainer/configs"
	"github.com/opencontainers/runc/libcontainer/system"
	"github.com/opencontainers/runc/libcontainer/utils"
)

// Synchronisation value for cgroup namespace setup.
// The same constant is defined in nsexec.c as "CREATECGROUPNS".
const createCgroupns byte = (1 << 7)

type parentProcess interface {
	// pid returns the pid for the running process.
	pid() int

	// start starts the process execution.
	start() error

	// send a SIGKILL to the process and wait for the exit.
	terminate() error

	// wait waits on the process returning the process state.
	wait() (*os.ProcessState, error)

	// startTime returns the process start time.
	startTime() (string, error)

	signal(os.Signal) error

	externalDescriptors() []string

	setExternalDescriptors(fds []string)
}

type setnsProcess struct {
	cmd           *exec.Cmd
	parentPipe    *os.File
	childPipe     *os.File
	cgroupPaths   map[string]string
	config        *initConfig
	fds           []string
	process       *Process
	bootstrapData io.Reader
}

func (p *setnsProcess) startTime() (string, error) {
	return system.GetProcessStartTime(p.pid())
}

func (p *setnsProcess) signal(sig os.Signal) error {
	s, ok := sig.(syscall.Signal)
	if !ok {
		return errors.New("os: unsupported signal type")
	}
	return syscall.Kill(p.pid(), s)
}

func (p *setnsProcess) start() (err error) {
	defer p.parentPipe.Close()
	err = p.cmd.Start()
	p.childPipe.Close()
	if err != nil {
		return newSystemErrorWithCause(err, "starting setns process")
	}
	if p.bootstrapData != nil {
		if _, err := io.Copy(p.parentPipe, p.bootstrapData); err != nil {
			return newSystemErrorWithCause(err, "copying bootstrap data to pipe")
		}
	}
	if err = p.execSetns(); err != nil {
		return newSystemErrorWithCause(err, "executing setns process")
	}
	if len(p.cgroupPaths) > 0 {
		if err := cgroups.EnterPid(p.cgroupPaths, p.pid()); err != nil {
			return newSystemErrorWithCausef(err, "adding pid %d to cgroups", p.pid())
		}
	}
	// set oom_score_adj
	if err := setOomScoreAdj(p.config.Config.OomScoreAdj, p.pid()); err != nil {
		return newSystemErrorWithCause(err, "setting oom score")
	}
	// set rlimits, this has to be done here because we lose permissions
	// to raise the limits once we enter a user-namespace
	if err := setupRlimits(p.config.Rlimits, p.pid()); err != nil {
		return newSystemErrorWithCause(err, "setting rlimits for process")
	}
	if err := utils.WriteJSON(p.parentPipe, p.config); err != nil {
		return newSystemErrorWithCause(err, "writing config to pipe")
	}

	ierr := parseSync(p.parentPipe, func(sync *syncT) error {
		switch sync.Type {
		case procConsole:
			if err := writeSync(p.parentPipe, procConsoleReq); err != nil {
				return newSystemErrorWithCause(err, "writing syncT 'request fd'")
			}

			masterFile, err := utils.RecvFd(p.parentPipe)
			if err != nil {
				return newSystemErrorWithCause(err, "getting master pty from child pipe")
			}

			if p.process.consoleChan == nil {
				// TODO: Don't panic here, do something more sane.
				panic("consoleChan is nil")
			}
			p.process.consoleChan <- masterFile

			if err := writeSync(p.parentPipe, procConsoleAck); err != nil {
				return newSystemErrorWithCause(err, "writing syncT 'ack fd'")
			}
		case procReady:
			// This shouldn't happen.
			panic("unexpected procReady in setns")
		case procHooks:
			// This shouldn't happen.
			panic("unexpected procHooks in setns")
		default:
			return newSystemError(fmt.Errorf("invalid JSON payload from child"))
		}
		return nil
	})

	if err := syscall.Shutdown(int(p.parentPipe.Fd()), syscall.SHUT_WR); err != nil {
		return newSystemErrorWithCause(err, "calling shutdown on init pipe")
	}
	// Must be done after Shutdown so the child will exit and we can wait for it.
	if ierr != nil {
		p.wait()
		return ierr
	}
	return nil
}

// execSetns runs the process that executes C code to perform the setns calls
// because setns support requires the C process to fork off a child and perform the setns
// before the go runtime boots, we wait on the process to die and receive the child's pid
// over the provided pipe.
func (p *setnsProcess) execSetns() error {
	status, err := p.cmd.Process.Wait()
	if err != nil {
		p.cmd.Wait()
		return newSystemErrorWithCause(err, "waiting on setns process to finish")
	}
	if !status.Success() {
		p.cmd.Wait()
		return newSystemError(&exec.ExitError{ProcessState: status})
	}
	var pid *pid
	if err := json.NewDecoder(p.parentPipe).Decode(&pid); err != nil {
		p.cmd.Wait()
		return newSystemErrorWithCause(err, "reading pid from init pipe")
	}
	process, err := os.FindProcess(pid.Pid)
	if err != nil {
		return err
	}
	p.cmd.Process = process
	p.process.ops = p
	return nil
}

// terminate sends a SIGKILL to the forked process for the setns routine then waits to
// avoid the process becoming a zombie.
func (p *setnsProcess) terminate() error {
	if p.cmd.Process == nil {
		return nil
	}
	err := p.cmd.Process.Kill()
	if _, werr := p.wait(); err == nil {
		err = werr
	}
	return err
}

func (p *setnsProcess) wait() (*os.ProcessState, error) {
	err := p.cmd.Wait()

	// Return actual ProcessState even on Wait error
	return p.cmd.ProcessState, err
}

func (p *setnsProcess) pid() int {
	return p.cmd.Process.Pid
}

func (p *setnsProcess) externalDescriptors() []string {
	return p.fds
}

func (p *setnsProcess) setExternalDescriptors(newFds []string) {
	p.fds = newFds
}

type initProcess struct {
	cmd           *exec.Cmd
	parentPipe    *os.File
	childPipe     *os.File
	config        *initConfig
	manager       cgroups.Manager
	container     *linuxContainer
	fds           []string
	process       *Process
	bootstrapData io.Reader
	sharePidns    bool
	rootDir       *os.File
}

func (p *initProcess) pid() int {
	return p.cmd.Process.Pid
}

func (p *initProcess) externalDescriptors() []string {
	return p.fds
}

// getChildPid receives the final child's pid over the provided pipe.
func (p *initProcess) getChildPid() (int, error) {
	var pid *pid
	if err := json.NewDecoder(p.parentPipe).Decode(&pid); err != nil {
		p.cmd.Wait()
		return -1, err
	}
	return pid.Pid, nil
}

func (p *initProcess) waitForChildExit(childPid int) error {
	status, err := p.cmd.Process.Wait()
	if err != nil {
		p.cmd.Wait()
		return err
	}
	if !status.Success() {
		p.cmd.Wait()
		return &exec.ExitError{ProcessState: status}
	}
	process, err := os.FindProcess(childPid)
	if err != nil {
		return err
	}
	p.cmd.Process = process
	p.process.ops = p
	return nil
}

func (p *initProcess) start() error {
	defer p.parentPipe.Close()
	err := p.cmd.Start()
	p.process.ops = p
	p.childPipe.Close()
	p.rootDir.Close()
	if err != nil {
		p.process.ops = nil
		return newSystemErrorWithCause(err, "starting init process command")
	}
	if _, err := io.Copy(p.parentPipe, p.bootstrapData); err != nil {
		return err
	}
	childPid, err := p.getChildPid()
	if err != nil {
		return newSystemErrorWithCause(err, "getting the final child's pid from pipe")
	}
	// Save the standard descriptor names before the container process
	// can potentially move them (e.g., via dup2()).  If we don't do this now,
	// we won't know at checkpoint time which file descriptor to look up.
	fds, err := getPipeFds(childPid)
	if err != nil {
		return newSystemErrorWithCausef(err, "getting pipe fds for pid %d", childPid)
	}
	p.setExternalDescriptors(fds)
	// Do this before syncing with child so that no children
	// can escape the cgroup
	if err := p.manager.Apply(childPid); err != nil {
		return newSystemErrorWithCause(err, "applying cgroup configuration for process")
	}
	// Now it's time to setup cgroup namesapce
	if p.config.Config.Namespaces.Contains(configs.NEWCGROUP) && p.config.Config.Namespaces.PathOf(configs.NEWCGROUP) == "" {
		if _, err := p.parentPipe.Write([]byte{createCgroupns}); err != nil {
			return newSystemErrorWithCause(err, "sending synchronization value to init process")
		}
	}

	// Wait for our first child to exit
	if err := p.waitForChildExit(childPid); err != nil {
		return newSystemErrorWithCause(err, "waiting for our first child to exit")
	}

	defer func() {
		if err != nil {
			// TODO: should not be the responsibility to call here
			p.manager.Destroy()
		}
	}()
	if err := p.createNetworkInterfaces(); err != nil {
		return newSystemErrorWithCause(err, "creating network interfaces")
	}
	if err := p.sendConfig(); err != nil {
		return newSystemErrorWithCause(err, "sending config to init process")
	}
	var (
		sentRun    bool
		sentResume bool
	)

	ierr := parseSync(p.parentPipe, func(sync *syncT) error {
		switch sync.Type {
		case procConsole:
			if err := writeSync(p.parentPipe, procConsoleReq); err != nil {
				return newSystemErrorWithCause(err, "writing syncT 'request fd'")
			}

			masterFile, err := utils.RecvFd(p.parentPipe)
			if err != nil {
				return newSystemErrorWithCause(err, "getting master pty from child pipe")
			}

			if p.process.consoleChan == nil {
				// TODO: Don't panic here, do something more sane.
				panic("consoleChan is nil")
			}
			p.process.consoleChan <- masterFile

			if err := writeSync(p.parentPipe, procConsoleAck); err != nil {
				return newSystemErrorWithCause(err, "writing syncT 'ack fd'")
			}
		case procReady:
			if err := p.manager.Set(p.config.Config); err != nil {
				return newSystemErrorWithCause(err, "setting cgroup config for ready process")
			}
			// set oom_score_adj
			if err := setOomScoreAdj(p.config.Config.OomScoreAdj, p.pid()); err != nil {
				return newSystemErrorWithCause(err, "setting oom score for ready process")
			}
			// set rlimits, this has to be done here because we lose permissions
			// to raise the limits once we enter a user-namespace
			if err := setupRlimits(p.config.Rlimits, p.pid()); err != nil {
				return newSystemErrorWithCause(err, "setting rlimits for ready process")
			}
			// call prestart hooks
			if !p.config.Config.Namespaces.Contains(configs.NEWNS) {
				if p.config.Config.Hooks != nil {
					s := configs.HookState{
						Version:    p.container.config.Version,
						ID:         p.container.id,
						Pid:        p.pid(),
						BundlePath: utils.SearchLabels(p.config.Config.Labels, "bundle"),
					}
					for i, hook := range p.config.Config.Hooks.Prestart {
						if err := hook.Run(s); err != nil {
							return newSystemErrorWithCausef(err, "running prestart hook %d", i)
						}
					}
				}
			}
			// Sync with child.
			if err := writeSync(p.parentPipe, procRun); err != nil {
				return newSystemErrorWithCause(err, "writing syncT 'run'")
			}
			sentRun = true
		case procHooks:
			if p.config.Config.Hooks != nil {
				s := configs.HookState{
					Version:    p.container.config.Version,
					ID:         p.container.id,
					Pid:        p.pid(),
					BundlePath: utils.SearchLabels(p.config.Config.Labels, "bundle"),
				}
				for i, hook := range p.config.Config.Hooks.Prestart {
					if err := hook.Run(s); err != nil {
						return newSystemErrorWithCausef(err, "running prestart hook %d", i)
					}
				}
			}
			// Sync with child.
			if err := writeSync(p.parentPipe, procResume); err != nil {
				return newSystemErrorWithCause(err, "writing syncT 'resume'")
			}
			sentResume = true
		default:
			return newSystemError(fmt.Errorf("invalid JSON payload from child"))
		}

		return nil
	})

	if !sentRun {
		return newSystemErrorWithCause(ierr, "container init")
	}
	if p.config.Config.Namespaces.Contains(configs.NEWNS) && !sentResume {
		return newSystemError(fmt.Errorf("could not synchronise after executing prestart hooks with container process"))
	}
	if err := syscall.Shutdown(int(p.parentPipe.Fd()), syscall.SHUT_WR); err != nil {
		return newSystemErrorWithCause(err, "shutting down init pipe")
	}

	// Must be done after Shutdown so the child will exit and we can wait for it.
	if ierr != nil {
		p.wait()
		return ierr
	}
	return nil
}

func (p *initProcess) wait() (*os.ProcessState, error) {
	err := p.cmd.Wait()
	if err != nil {
		return p.cmd.ProcessState, err
	}
	// we should kill all processes in cgroup when init is died if we use host PID namespace
	if p.sharePidns {
		signalAllProcesses(p.manager, syscall.SIGKILL)
	}
	return p.cmd.ProcessState, nil
}

func (p *initProcess) terminate() error {
	if p.cmd.Process == nil {
		return nil
	}
	err := p.cmd.Process.Kill()
	if _, werr := p.wait(); err == nil {
		err = werr
	}
	return err
}

func (p *initProcess) startTime() (string, error) {
	return system.GetProcessStartTime(p.pid())
}

func (p *initProcess) sendConfig() error {
	// send the config to the container's init process, we don't use JSON Encode
	// here because there might be a problem in JSON decoder in some cases, see:
	// https://github.com/docker/docker/issues/14203#issuecomment-174177790
	return utils.WriteJSON(p.parentPipe, p.config)
}

func (p *initProcess) createNetworkInterfaces() error {
	for _, config := range p.config.Config.Networks {
		strategy, err := getStrategy(config.Type)
		if err != nil {
			return err
		}
		n := &network{
			Network: *config,
		}
		if err := strategy.create(n, p.pid()); err != nil {
			return err
		}
		p.config.Networks = append(p.config.Networks, n)
	}
	return nil
}

func (p *initProcess) signal(sig os.Signal) error {
	s, ok := sig.(syscall.Signal)
	if !ok {
		return errors.New("os: unsupported signal type")
	}
	return syscall.Kill(p.pid(), s)
}

func (p *initProcess) setExternalDescriptors(newFds []string) {
	p.fds = newFds
}

func getPipeFds(pid int) ([]string, error) {
	fds := make([]string, 3)

	dirPath := filepath.Join("/proc", strconv.Itoa(pid), "/fd")
	for i := 0; i < 3; i++ {
		// XXX: This breaks if the path is not a valid symlink (which can
		//      happen in certain particularly unlucky mount namespace setups).
		f := filepath.Join(dirPath, strconv.Itoa(i))
		target, err := os.Readlink(f)
		if err != nil {
			return fds, err
		}
		fds[i] = target
	}
	return fds, nil
}

// InitializeIO creates pipes for use with the process's stdio and returns the
// opposite side for each. Do not use this if you want to have a pseudoterminal
// set up for you by libcontainer (TODO: fix that too).
// TODO: This is mostly unnecessary, and should be handled by clients.
func (p *Process) InitializeIO(rootuid, rootgid int) (i *IO, err error) {
	var fds []uintptr
	i = &IO{}
	// cleanup in case of an error
	defer func() {
		if err != nil {
			for _, fd := range fds {
				syscall.Close(int(fd))
			}
		}
	}()
	// STDIN
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	fds = append(fds, r.Fd(), w.Fd())
	p.Stdin, i.Stdin = r, w
	// STDOUT
	if r, w, err = os.Pipe(); err != nil {
		return nil, err
	}
	fds = append(fds, r.Fd(), w.Fd())
	p.Stdout, i.Stdout = w, r
	// STDERR
	if r, w, err = os.Pipe(); err != nil {
		return nil, err
	}
	fds = append(fds, r.Fd(), w.Fd())
	p.Stderr, i.Stderr = w, r
	// change ownership of the pipes incase we are in a user namespace
	for _, fd := range fds {
		if err := syscall.Fchown(int(fd), rootuid, rootgid); err != nil {
			return nil, err
		}
	}
	return i, nil
}
