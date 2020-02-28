package ctrd

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/alibaba/pouch/apis/types"
	"github.com/alibaba/pouch/daemon/containerio"
	"github.com/alibaba/pouch/pkg/errtypes"
	"github.com/alibaba/pouch/pkg/exec"
	"github.com/alibaba/pouch/pkg/ioutils"
	"github.com/alibaba/pouch/pkg/log"

	"github.com/containerd/containerd"
	containerdtypes "github.com/containerd/containerd/api/types"
	"github.com/containerd/containerd/cio"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/leases"
	"github.com/containerd/containerd/oci"
	"github.com/pkg/errors"
)

var (
	// RuntimeRoot is the base directory path for each runtime.
	RuntimeRoot = "/run"
	// RuntimeTypeV1 is the runtime type name for containerd shim interface v1 version.
	RuntimeTypeV1 = fmt.Sprintf("io.containerd.runtime.v1.%s", runtime.GOOS)
	// RuntimeTypeV2runscV1 is the runtime type name for gVisor containerd shim implement the shim v2 api.
	RuntimeTypeV2runscV1 = "io.containerd.runsc.v1"
	// RuntimeTypeV2kataV2 is the runtime type name for kata-runtime containerd shim implement the shim v2 api.
	RuntimeTypeV2kataV2 = "io.containerd.kata.v2"
	// RuntimeTypeV2runcV1 is the runtime type name for runc containerd shim implement the shim v2 api.
	RuntimeTypeV2runcV1 = "io.containerd.runc.v1"

	// cleanupTimeout is used to clean up the container/task meta data in containerd.
	cleanupTimeout = 100 * time.Second
)

type containerPack struct {
	id        string
	ch        chan *Message
	sch       <-chan containerd.ExitStatus
	container containerd.Container
	task      containerd.Task

	// client is to record which stream client the container connect with
	client *WrapperClient
}

// ContainerStats returns stats of the container.
func (c *Client) ContainerStats(ctx context.Context, id string) (*containerdtypes.Metric, error) {
	metric, err := c.containerStats(ctx, id)
	if err != nil {
		return metric, convertCtrdErr(err)
	}
	return metric, nil
}

// containerStats returns stats of the container.
func (c *Client) containerStats(ctx context.Context, id string) (*containerdtypes.Metric, error) {
	if !c.lock.TrylockWithRetry(ctx, id) {
		return nil, errtypes.ErrLockfailed
	}
	defer c.lock.Unlock(id)

	pack, err := c.watch.get(id)
	if err != nil {
		return nil, err
	}

	metrics, err := pack.task.Metrics(ctx)
	if err != nil {
		return nil, err
	}

	return metrics, nil
}

// ExecContainer executes a process in container.
func (c *Client) ExecContainer(ctx context.Context, process *Process, timeout int) error {
	if err := c.execContainer(ctx, process, timeout); err != nil {
		return convertCtrdErr(err)
	}
	return nil
}

// execContainer executes a process in container.
func (c *Client) execContainer(ctx context.Context, process *Process, timeout int) error {
	pack, err := c.watch.get(process.ContainerID)
	if err != nil {
		return err
	}

	closeStdinCh := make(chan struct{})

	var (
		cntrID, execID          = pack.container.ID(), process.ExecID
		withStdin, withTerminal = process.IO.Stream().Stdin() != nil, process.P.Terminal
		msg                     *Message
	)

	// create exec process in container
	execProcess, err := pack.task.Exec(ctx, process.ExecID, process.P, func(_ string) (cio.IO, error) {
		log.With(ctx).Debugf("creating cio (withStdin=%v, withTerminal=%v), process(%s)", withStdin, withTerminal, execID)

		fifoset, err := containerio.NewFIFOSet(execID, withStdin, withTerminal)
		if err != nil {
			return nil, err
		}
		return c.createIO(fifoset, cntrID, execID, closeStdinCh, process.IO.InitContainerIO)
	})
	if err != nil {
		return errors.Wrap(err, "failed to exec process")
	}

	// wait exec process to exit
	exitStatus, err := execProcess.Wait(context.TODO())
	if err != nil {
		return errors.Wrap(err, "failed to exec process")
	}

	cleanup := func(msg *Message) {
		if msg == nil {
			return
		}
		// XXX: if exec process get run, io should be closed in this function,
		for _, hook := range c.hooks {
			if err := hook(process.ExecID, msg); err != nil {
				log.With(ctx).Errorf("failed to execute the exec exit hooks: %v", err)
				break
			}
		}

		// delete the finished exec process in containerd
		if _, err := execProcess.Delete(context.TODO()); err != nil {
			log.With(ctx).Warnf("failed to delete exec process %s: %s", process.ExecID, err)
		}
	}
	// start the exec process
	if err := execProcess.Start(ctx); err != nil {
		close(closeStdinCh)

		// delete exec process in containerd to cleanup pipe fd
		if _, cerr := execProcess.Delete(context.TODO()); cerr != nil {
			log.With(ctx).Warnf("failed to delete exec process %s: %s", process.ExecID, cerr)
		}
		return errors.Wrapf(err, "failed to start exec, exec id %s", execID)
	}
	// make sure the closeStdinCh has been closed.
	close(closeStdinCh)

	if process.Detach {
		go func() {
			status := <-exitStatus
			cleanup(&Message{
				err:      status.Error(),
				exitCode: status.ExitCode(),
				exitTime: status.ExitTime(),
			})
		}()
		return nil
	}

	defer func() {
		cleanup(msg)
	}()

	t := time.Duration(timeout) * time.Second
	var timeCh <-chan time.Time
	if t == 0 {
		timeCh = make(chan time.Time)
	} else {
		timeCh = time.After(t)
	}

	select {
	case status := <-exitStatus:
		msg = &Message{
			err:      status.Error(),
			exitCode: status.ExitCode(),
			exitTime: status.ExitTime(),
		}
	case <-timeCh:
		// ignore the not found error because the process may exit itself before kill
		if err := execProcess.Kill(ctx, syscall.SIGKILL); err != nil && !errdefs.IsNotFound(err) {
			// try to force kill the exec process
			if err := execProcess.Kill(ctx, syscall.SIGTERM); err != nil && !errdefs.IsNotFound(err) {
				return errors.Wrapf(err, "failed to kill the exec process")
			}
		}
		// wait for process to be killed
		status := <-exitStatus
		msg = &Message{
			err:      errors.Wrapf(status.Error(), "failed to exec process %s, timeout", execID),
			exitCode: status.ExitCode(),
			exitTime: status.ExitTime(),
		}
	}

	return nil
}

// ResizeExec changes the size of the TTY of the exec process running
// in the container to the given height and width.
func (c *Client) ResizeExec(ctx context.Context, id string, execid string, opts types.ResizeOptions) error {
	pack, err := c.watch.get(id)
	if err != nil {
		return err
	}

	execProcess, err := pack.task.LoadProcess(ctx, execid, nil)
	if err != nil {
		return err
	}

	return execProcess.Resize(ctx, uint32(opts.Width), uint32(opts.Height))
}

// ContainerPID returns the container's init process id.
func (c *Client) ContainerPID(ctx context.Context, id string) (int, error) {
	pid, err := c.containerPID(ctx, id)
	if err != nil {
		return pid, convertCtrdErr(err)
	}
	return pid, nil
}

// containerPID returns the container's init process id.
func (c *Client) containerPID(ctx context.Context, id string) (int, error) {
	pack, err := c.watch.get(id)
	if err != nil {
		return -1, err
	}
	return int(pack.task.Pid()), nil
}

// ContainerPIDs returns the all processes's ids inside the container.
func (c *Client) ContainerPIDs(ctx context.Context, id string) ([]containerd.ProcessInfo, error) {
	pids, err := c.containerPIDs(ctx, id)
	if err != nil {
		return pids, convertCtrdErr(err)
	}
	return pids, nil
}

// containerPIDs returns the all processes's ids inside the container.
func (c *Client) containerPIDs(ctx context.Context, id string) ([]containerd.ProcessInfo, error) {
	if !c.lock.TrylockWithRetry(ctx, id) {
		return nil, errtypes.ErrLockfailed
	}
	defer c.lock.Unlock(id)

	pack, err := c.watch.get(id)
	if err != nil {
		return nil, err
	}

	processes, err := pack.task.Pids(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get task's pids")
	}

	return processes, nil
}

// ContainerStatus returns the status of container.
func (c *Client) ContainerStatus(ctx context.Context, id string) (containerd.Status, error) {
	status, err := c.containerStatus(ctx, id)
	if err != nil {
		return status, convertCtrdErr(err)
	}
	return status, nil
}

// containerStatus returns the status of container.
func (c *Client) containerStatus(ctx context.Context, id string) (containerd.Status, error) {
	if !c.lock.TrylockWithRetry(ctx, id) {
		return containerd.Status{}, errtypes.ErrLockfailed
	}
	defer c.lock.Unlock(id)

	pack, err := c.watch.get(id)
	if err != nil {
		return containerd.Status{}, err
	}

	status, err := pack.task.Status(ctx)
	if err != nil {
		return containerd.Status{}, errors.Wrap(err, "failed to get task's status")
	}
	return status, nil
}

// ProbeContainer probe the container's status, if timeout <= 0, will block to receive message.
func (c *Client) ProbeContainer(ctx context.Context, id string, timeout time.Duration) *Message {
	ch := c.watch.notify(id)

	if timeout <= 0 {
		msg := <-ch
		ch <- msg // put it back, make sure the method can be called repeatedly.

		return msg
	}
	select {
	case msg := <-ch:
		ch <- msg // put it back, make sure the method can be called repeatedly.
		return msg
	case <-time.After(timeout):
		return &Message{err: errtypes.ErrTimeout}
	case <-ctx.Done():
		return &Message{err: ctx.Err()}
	}
}

// RecoverContainer reload the container from metadata and watch it, if program be restarted.
func (c *Client) RecoverContainer(ctx context.Context, id string, io *containerio.IO) error {
	if err := c.recoverContainer(ctx, id, io); err != nil {
		return convertCtrdErr(err)
	}
	return nil
}

// recoverContainer reload the container from metadata and watch it, if program be restarted.
func (c *Client) recoverContainer(ctx context.Context, id string, io *containerio.IO) (err0 error) {
	wrapperCli, err := c.Get(ctx)
	if err != nil {
		return fmt.Errorf("failed to get a containerd grpc client: %v", err)
	}

	if !c.lock.TrylockWithRetry(ctx, id) {
		return errtypes.ErrLockfailed
	}
	defer c.lock.Unlock(id)

	lc, err := wrapperCli.client.LoadContainer(ctx, id)
	if err != nil {
		log.With(ctx).Errorf("failed to load container from containerd: %v", err)

		if errdefs.IsNotFound(err) {
			return errors.Wrapf(errtypes.ErrNotfound, "container %s", id)
		}
		return errors.Wrapf(err, "failed to load container(%s)", id)
	}

	var (
		timeout = 3 * time.Second
		ch      = make(chan error, 1)
		task    containerd.Task
	)

	// for normal shim, this operation should be end less than 1 second,
	// we give 5 second timeout to believe the shim get locked internal,
	// return error since we do not want a hang shim affect daemon start
	// XXX: when system load is high, make connect to shim fail on retry 3 times
	for i := 0; i < 3; i++ {
		pctx, cancel := context.WithCancel(ctx)
		defer cancel()
		go func() {
			task, err = lc.Task(pctx, func(fset *cio.FIFOSet) (cio.IO, error) {
				return c.attachIO(fset, io.InitContainerIO)
			})
			ch <- err
		}()

		select {
		case <-time.After(timeout):
			if i < 2 {
				log.With(ctx).Warn("timeout connect to shim, retry")
				continue
			}
			return errors.Wrap(errtypes.ErrTimeout, "failed to connect to shim")
		case err = <-ch:
		}

		break
	}

	if err != nil {
		log.With(ctx).Errorf("failed to get task from containerd: %v", err)

		if !errdefs.IsNotFound(err) {
			return errors.Wrap(err, "failed to get task")
		}
		// not found task, delete container directly.
		lc.Delete(ctx)
		return errors.Wrap(errtypes.ErrNotfound, "task")
	}

	statusCh, err := task.Wait(ctx)
	if err != nil {
		return errors.Wrap(err, "failed to wait task")
	}

	c.watch.add(ctx, &containerPack{
		id:        id,
		container: lc,
		task:      task,
		ch:        make(chan *Message, 1),
		client:    wrapperCli,
		sch:       statusCh,
	})

	log.With(ctx).Infof("success to recover container")
	return nil
}

// DestroyContainer kill container and delete it.
func (c *Client) DestroyContainer(ctx context.Context, id string, timeout int64) error {
	return convertCtrdErr(c.destroyContainer(ctx, id, timeout))
}

// destroyContainer kill container and delete it.
func (c *Client) destroyContainer(ctx context.Context, id string, timeout int64) error {
	var err error
	// TODO(ziren): if we just want to stop a container,
	// we may need lease to lock the snapshot of container,
	// in case, it be deleted by gc.
	wrapperCli, err := c.Get(ctx)
	if err != nil {
		return fmt.Errorf("failed to get a containerd grpc client: %v", err)
	}

	ctx = leases.WithLease(ctx, wrapperCli.lease.ID)

	if !c.lock.TrylockWithRetry(ctx, id) {
		return errtypes.ErrLockfailed
	}
	defer c.lock.Unlock(id)

	pack, err := c.watch.get(id)
	if err != nil {
		if err = c.forceDestroyContainer(ctx, id); err != nil {
			return err
		}

		return nil
	}

	waitExit := func() *Message {
		return c.ProbeContainer(ctx, id, time.Duration(timeout)*time.Second)
	}

	var msg *Message

	// TODO: set task request timeout by context timeout
	nCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	if err = pack.task.Kill(nCtx, syscall.SIGTERM, containerd.WithKillAll); err != nil {
		if errdefs.IsNotFound(err) {
			log.With(ctx).Warnf("killing task is not fond")
			return err
		}

		log.With(ctx).Warnf("failed to send sigterm(15) to kill, try to send sigkill(9) to container again, err(%v)", err)

		// retry with kill 9
		nCtxForce, cancelForce := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
		defer cancelForce()
		if err = pack.task.Kill(nCtxForce, syscall.SIGKILL, containerd.WithKillAll); err != nil {
			if errdefs.IsNotFound(err) {
				log.With(ctx).Warnf("killing task is not fond")
				return err
			}

			log.With(ctx).Warnf("failed to send sigkill(9) to kill, err(%v)", err)

			// force to kill containerd-shim and all container's threads
			if err = c.forceDestroyContainer(ctx, pack.id); err != nil {
				return errors.Wrap(err, "failed to force destroy container")
			}
		}
	}

	// wait for the task to exit.
	msg = waitExit()

	if err = msg.RawError(); err != nil && errtypes.IsTimeout(err) {
		log.With(ctx).Warnf("send sigterm timeout %d s, send signal 9 to container", timeout)

		// timeout, use SIGKILL to retry.
		if err = pack.task.Kill(ctx, syscall.SIGKILL, containerd.WithKillAll); err != nil {
			if errdefs.IsNotFound(err) {
				log.With(ctx).Warnf("killing task is not fond")
				return err
			}

			log.With(ctx).Warnf("failed to send sigkill(9) to kill, err(%v)", err)

			// force to kill containerd-shim and all container's threads
			if err = c.forceDestroyContainer(ctx, pack.id); err != nil {
				return errors.Wrap(err, "failed to force destroy container")
			}

			return err
		}
		msg = waitExit()
	}

	// if killing task fails, delete task is meaningless, just return error here.
	if err = msg.RawError(); err != nil {
		return err
	}

	return nil
}

func (c *Client) forceDestroyContainer(ctx context.Context, id string) error {
	log.With(ctx).Infof("force to kill containerd-shim, containerID(%s)", id)

	exit, stdout, stderr, err := exec.Run(60*time.Second, "/opt/ali-iaas/pouch/bin/make_sure_stop.sh", id)
	return errors.Wrapf(err, "failed to run make_sure_stop.sh, exit(%d), stdout(%s), stderr(%s)",
		exit, stdout, stderr)
}

// PauseContainer pauses container.
func (c *Client) PauseContainer(ctx context.Context, id string) error {
	if err := c.pauseContainer(ctx, id); err != nil {
		return convertCtrdErr(err)
	}
	return nil
}

// pauseContainer pause container.
func (c *Client) pauseContainer(ctx context.Context, id string) error {
	if !c.lock.TrylockWithRetry(ctx, id) {
		return errtypes.ErrLockfailed
	}
	defer c.lock.Unlock(id)

	pack, err := c.watch.get(id)
	if err != nil {
		return err
	}

	if err := pack.task.Pause(ctx); err != nil {
		if !errdefs.IsNotFound(err) {
			return errors.Wrap(err, "failed to pause task")
		}
	}

	log.With(ctx).Infof("success to pause container")

	return nil
}

// UnpauseContainer unpauses container.
func (c *Client) UnpauseContainer(ctx context.Context, id string) error {
	if err := c.unpauseContainer(ctx, id); err != nil {
		return convertCtrdErr(err)
	}
	return nil
}

// unpauseContainer unpauses a container.
func (c *Client) unpauseContainer(ctx context.Context, id string) error {
	if !c.lock.TrylockWithRetry(ctx, id) {
		return errtypes.ErrLockfailed
	}
	defer c.lock.Unlock(id)

	pack, err := c.watch.get(id)
	if err != nil {
		return err
	}

	if err := pack.task.Resume(ctx); err != nil {
		if !errdefs.IsNotFound(err) {
			return errors.Wrap(err, "failed to resume task")
		}
	}

	log.With(ctx).Infof("success to unpause container")

	return nil
}

// CreateContainer create container and start process.
func (c *Client) CreateContainer(ctx context.Context, container *Container, checkpointDir string) error {
	var id = container.ID

	if !c.lock.TrylockWithRetry(ctx, id) {
		return errtypes.ErrLockfailed
	}
	defer c.lock.Unlock(id)

	if err := c.createContainer(ctx, id, checkpointDir, container); err != nil {
		return convertCtrdErr(err)
	}
	return nil
}

func (c *Client) createContainer(ctx context.Context, id, checkpointDir string, container *Container) (err0 error) {
	wrapperCli, err := c.Get(ctx)
	if err != nil {
		return fmt.Errorf("failed to get a containerd grpc client: %v", err)
	}

	// create container
	options := []containerd.NewContainerOpts{
		containerd.WithSnapshotter(CurrentSnapshotterName(ctx)),
		containerd.WithContainerLabels(container.Labels),
		containerd.WithRuntime(container.RuntimeType, container.RuntimeOptions),
	}

	rootFSPath := "rootfs"
	// if container is taken over by pouch, not created by pouch
	if container.RootFSProvided {
		rootFSPath = container.BaseFS
	} else { // containers created by pouch must first create snapshot
		// check snapshot exist or not.
		if _, err := c.GetSnapshot(ctx, container.SnapshotID); err != nil {
			return errors.Wrapf(err, "failed to create container %s", id)
		}
		options = append(options, containerd.WithSnapshot(container.SnapshotID))
	}

	// specify Spec for new container
	specOptions := []oci.SpecOpts{
		oci.WithRootFSPath(rootFSPath),
	}
	options = append(options, containerd.WithSpec(container.Spec, specOptions...))

	nc, err := wrapperCli.client.NewContainer(ctx, id, options...)
	if err != nil {
		return errors.Wrapf(err, "failed to create container %s", id)
	}

	defer func() {
		if err0 != nil {
			// Delete snapshot when start failed, may cause data lost.
			dctx, dcancel := context.WithTimeout(context.TODO(), cleanupTimeout)
			defer dcancel()
			if cerr := nc.Delete(dctx); cerr != nil {
				log.With(ctx).Warnf("failed to cleanup container(id=%s) meta in containerd: %v", nc.ID(), cerr)
			}
		}
	}()

	log.With(ctx).Infof("success to new container")

	// create task
	pack, err := c.createTask(ctx, container.RuntimeType, id, checkpointDir, nc, container, wrapperCli.client)
	if err != nil {
		return err
	}

	// add grpc client to pack struct
	pack.client = wrapperCli

	c.watch.add(ctx, pack)

	return nil
}

func (c *Client) createTask(ctx context.Context, runtime, id, checkpointDir string, container containerd.Container, cc *Container, client *containerd.Client) (p *containerPack, err0 error) {

	var (
		pack                    *containerPack
		cntrID, execID          = id, id
		withStdin, withTerminal = cc.IO.Stream().Stdin() != nil, cc.Spec.Process.Terminal
		closeStdinCh            = make(chan struct{})
	)

	// create task
	task, err := container.NewTask(ctx, func(_ string) (cio.IO, error) {
		log.With(ctx).Debugf("creating cio (withStdin=%v, withTerminal=%v)", withStdin, withTerminal)

		fifoset, err := containerio.NewFIFOSet(execID, withStdin, withTerminal)
		if err != nil {
			return nil, err
		}
		return c.createIO(fifoset, cntrID, execID, closeStdinCh, cc.IO.InitContainerIO)
	}, withRestoreOpts(runtime, checkpointDir))
	close(closeStdinCh)

	if err != nil {
		return pack, errors.Wrapf(err, "failed to create task for container(%s)", id)
	}

	defer func() {
		if err0 != nil {
			dctx, dcancel := context.WithTimeout(context.TODO(), cleanupTimeout)
			defer dcancel()

			if _, cerr := task.Delete(dctx, containerd.WithProcessKill); cerr != nil {
				log.With(ctx).Warnf("failed to cleanup task(id=%s) meta in containerd: %v", task.ID(), cerr)
			}
		}
	}()

	statusCh, err := task.Wait(context.TODO())
	if err != nil {
		return pack, errors.Wrapf(err, "failed to wait task in container(%s)", id)
	}

	log.With(ctx).Infof("success to create task(pid=%d)", task.Pid())

	// start task
	if err := task.Start(ctx); err != nil {
		return pack, errors.Wrapf(err, "failed to start task(%d) in container(%s)", task.Pid(), id)
	}

	log.With(ctx).Infof("success to start task")

	pack = &containerPack{
		id:        id,
		container: container,
		task:      task,
		ch:        make(chan *Message, 1),
		sch:       statusCh,
	}

	return pack, nil
}

// UpdateResources updates the configurations of a container.
func (c *Client) UpdateResources(ctx context.Context, id string, resources types.Resources) error {
	if err := c.updateResources(ctx, id, resources); err != nil {
		return convertCtrdErr(err)
	}
	return nil
}

// updateResources updates the configurations of a container.
func (c *Client) updateResources(ctx context.Context, id string, resources types.Resources) error {
	if !c.lock.TrylockWithRetry(ctx, id) {
		return errtypes.ErrLockfailed
	}
	defer c.lock.Unlock(id)

	pack, err := c.watch.get(id)
	if err != nil {
		return err
	}

	r, err := toLinuxResources(resources)
	if err != nil {
		return err
	}

	return pack.task.Update(ctx, containerd.WithResources(r))
}

// ResizeContainer changes the size of the TTY of the init process running
// in the container to the given height and width.
func (c *Client) ResizeContainer(ctx context.Context, id string, opts types.ResizeOptions) error {
	if err := c.resizeContainer(ctx, id, opts); err != nil {
		return convertCtrdErr(err)
	}
	return nil
}

// resizeContainer changes the size of the TTY of the init process running
// in the container to the given height and width.
func (c *Client) resizeContainer(ctx context.Context, id string, opts types.ResizeOptions) error {
	if !c.lock.TrylockWithRetry(ctx, id) {
		return errtypes.ErrLockfailed
	}
	defer c.lock.Unlock(id)

	pack, err := c.watch.get(id)
	if err != nil {
		return err
	}

	return pack.task.Resize(ctx, uint32(opts.Width), uint32(opts.Height))
}

// WaitContainer waits until container's status is stopped.
func (c *Client) WaitContainer(ctx context.Context, id string) (types.ContainerWaitOKBody, error) {
	waitBody, err := c.waitContainer(ctx, id)
	if err != nil {
		return waitBody, convertCtrdErr(err)
	}
	return waitBody, nil
}

// waitContainer waits until container's status is stopped.
func (c *Client) waitContainer(ctx context.Context, id string) (types.ContainerWaitOKBody, error) {
	wrapperCli, err := c.Get(ctx)
	if err != nil {
		return types.ContainerWaitOKBody{}, fmt.Errorf("failed to get a containerd grpc client: %v", err)
	}

	ctx = leases.WithLease(ctx, wrapperCli.lease.ID)

	waitExit := func() *Message {
		return c.ProbeContainer(ctx, id, -1*time.Second)
	}

	// wait for the task to exit.
	msg := waitExit()

	errMsg := ""
	err = msg.RawError()
	if err != nil {
		if errtypes.IsTimeout(err) {
			return types.ContainerWaitOKBody{}, err
		}
		errMsg = err.Error()
	}

	return types.ContainerWaitOKBody{
		Error:      errMsg,
		StatusCode: int64(msg.ExitCode()),
	}, nil
}

// CreateCheckpoint create a checkpoint from a running container
func (c *Client) CreateCheckpoint(ctx context.Context, runtime, id string, checkpointDir string, exit bool) error {
	pack, err := c.watch.get(id)
	if err != nil {
		return err
	}

	opts := []containerd.CheckpointTaskOpts{withCheckpointOpts(runtime, checkpointDir, exit)}
	_, err = pack.task.Checkpoint(ctx, opts...)
	if err != nil {
		return fmt.Errorf("failed to checkpoint: %s", err)
	}
	return nil
}

// InitStdio allows caller to handle any initialize job.
type InitStdio func(dio *cio.DirectIO) (cio.IO, error)

func (c *Client) createIO(fifoSet *cio.FIFOSet, cntrID, procID string, closeStdinCh <-chan struct{}, initstdio InitStdio) (cio.IO, error) {
	cdio, err := cio.NewDirectIO(context.Background(), fifoSet)
	if err != nil {
		return nil, err
	}

	if cdio.Stdin != nil {
		var (
			errClose  error
			stdinOnce sync.Once
		)
		oldStdin := cdio.Stdin
		cdio.Stdin = ioutils.NewWriteCloserWrapper(oldStdin, func() error {
			stdinOnce.Do(func() {
				errClose = oldStdin.Close()

				// Both the caller and container/exec process holds write side pipe
				// for the stdin. When the caller closes the write pipe, the process doesn't
				// exit until the caller calls the CloseIO.
				go func() {
					<-closeStdinCh
					if err := c.closeStdinIO(cntrID, procID); err != nil {
						// TODO(fuweid): for the CloseIO grpc call, the containerd doesn't
						// return correct status code if the process doesn't exist.
						// for the case, we should use strings.Contains to reduce warning
						// log. it will be fixed in containerd#2747.
						if !errdefs.IsNotFound(err) && !strings.Contains(err.Error(), "not found") {
							log.With(nil).WithError(err).Warnf("failed to close stdin containerd IO (container:%v, process:%v", cntrID, procID)
						}
					}
				}()
			})
			return errClose
		})
	}

	cntrio, err := initstdio(cdio)
	if err != nil {
		cdio.Cancel()
		cdio.Close()
		return nil, err
	}
	return cntrio, nil
}

func (c *Client) attachIO(fifoSet *cio.FIFOSet, initstdio InitStdio) (cio.IO, error) {
	if fifoSet == nil {
		return nil, fmt.Errorf("cannot attach to existing fifos")
	}

	cdio, err := cio.NewDirectIO(context.Background(), &cio.FIFOSet{
		Config: cio.Config{
			Terminal: fifoSet.Terminal,
			Stdin:    fifoSet.Stdin,
			Stdout:   fifoSet.Stdout,
			Stderr:   fifoSet.Stderr,
		},
	})
	if err != nil {
		return nil, err
	}

	cntrio, err := initstdio(cdio)
	if err != nil {
		cdio.Cancel()
		cdio.Close()
		return nil, err
	}
	return cntrio, nil
}

// closeStdinIO is used to close the write side of fifo in containerd-shim.
//
// NOTE: we should use client to make rpc call directly. if we retrieve it from
// watch, it might return 404 because the pack is saved into cache after Start.
func (c *Client) closeStdinIO(containerID, processID string) error {
	ctx := context.Background()
	wrapperCli, err := c.Get(ctx)
	if err != nil {
		return fmt.Errorf("failed to get a containerd grpc client: %v", err)
	}

	cli := wrapperCli.client
	cntr, err := cli.LoadContainer(ctx, containerID)
	if err != nil {
		return err
	}

	t, err := cntr.Task(ctx, nil)
	if err != nil {
		return err
	}

	p, err := t.LoadProcess(ctx, processID, nil)
	if err != nil {
		return err
	}

	return p.CloseIO(ctx, containerd.WithStdinCloser)
}
