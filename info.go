package deskconn

import (
	"context"
	"encoding/json"
	"math"

	"github.com/xconnio/deskconn/info"
	"github.com/xconnio/xconn-go"
)

const (
	ProcedureDeviceInfo      = "io.xconn.deskconn.deskconnd.device.info"
	ProcedureDeviceIsDesktop = "io.xconn.deskconn.deskconnd.device.is_desktop"
	ProcedureProcessList     = "io.xconn.deskconn.deskconnd.process.list"
	ProcedureProcessSignal   = "io.xconn.deskconn.deskconnd.process.signal"
	ProcedureAppList         = "io.xconn.deskconn.deskconnd.app.list"
	ProcedureAppIcon         = "io.xconn.deskconn.deskconnd.app.icon"
)

func (d *Deskconn) handleDeviceInfo(_ context.Context, _ *xconn.Invocation) *xconn.InvocationResult {
	deviceInfo, err := info.GetDeviceInfo()
	if err != nil {
		return xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}

	data, err := json.Marshal(deviceInfo)
	if err != nil {
		return xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}

	return xconn.NewInvocationResult(data)
}

func (d *Deskconn) handleDeviceIsDesktop(_ context.Context, _ *xconn.Invocation) *xconn.InvocationResult {
	return xconn.NewInvocationResult(d.desktop)
}

func (d *Deskconn) handleProcessList(_ context.Context, _ *xconn.Invocation) *xconn.InvocationResult {
	infos, err := d.processes.List()
	if err != nil {
		return xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}

	data, err := json.Marshal(infos)
	if err != nil {
		return xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}

	return xconn.NewInvocationResult(data)
}

func (d *Deskconn) handleProcessSignal(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
	pidArgs, err := inv.ArgList(0)
	if err != nil {
		return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
	}
	sig, err := inv.ArgString(1)
	if err != nil {
		return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
	}

	pids := make([]int32, 0, pidArgs.Len())
	for i := 0; i < pidArgs.Len(); i++ {
		pid64, err := pidArgs.Int64(i)
		if err != nil {
			return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
		}
		if pid64 < 0 || pid64 > math.MaxInt32 {
			return xconn.NewInvocationError(ErrInvalidArgument, "pid out of range")
		}
		pids = append(pids, int32(pid64))
	}

	results, err := info.Signal(pids, sig)
	if err != nil {
		return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
	}

	data, err := json.Marshal(results)
	if err != nil {
		return xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}

	return xconn.NewInvocationResult(data)
}

func (d *Deskconn) handleAppList(_ context.Context, _ *xconn.Invocation) *xconn.InvocationResult {
	procs, err := d.processes.List()
	if err != nil {
		return xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}

	apps := d.appRegistry.Build(procs)

	data, err := json.Marshal(apps)
	if err != nil {
		return xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}

	return xconn.NewInvocationResult(data)
}

func (d *Deskconn) handleAppIcon(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
	name, err := inv.ArgString(0)
	if err != nil {
		return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
	}

	mimeType, data, err := info.ReadIcon(name)
	if err != nil {
		return xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}

	result := struct {
		Mime string `json:"mime"`
		Data []byte `json:"data"`
	}{Mime: mimeType, Data: data}

	payload, err := json.Marshal(result)
	if err != nil {
		return xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}

	return xconn.NewInvocationResult(payload)
}
