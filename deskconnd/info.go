package deskconnd

import (
	"context"
	"math"

	"github.com/xconnio/deskconn/common"
	"github.com/xconnio/xconn-go"
)

func (d *Deskconn) handleMachineInfo(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
	enc, errResult := d.callerKeys(inv)
	if errResult != nil {
		return errResult
	}

	machineInfo, err := GetMachineInfo()
	if err != nil {
		return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
	}
	return encryptResult(enc, machineInfo)
}

func (d *Deskconn) handleMachineIsDesktop(_ context.Context, _ *xconn.Invocation) *xconn.InvocationResult {
	return xconn.NewInvocationResult(d.desktop)
}

func (d *Deskconn) handleProcessList(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
	enc, errResult := d.callerKeys(inv)
	if errResult != nil {
		return errResult
	}

	infos, err := d.processes.List()
	if err != nil {
		return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
	}
	return encryptResult(enc, infos)
}

// processSignalArgs is ProcedureProcessSignal's argument.
type processSignalArgs struct {
	PIDs   []int64 `json:"pids"`
	Signal string  `json:"signal"`
}

func (d *Deskconn) handleProcessSignal(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
	enc, errResult := d.callerKeys(inv)
	if errResult != nil {
		return errResult
	}

	var args processSignalArgs
	if errResult = decryptArgs(enc, inv, &args); errResult != nil {
		return errResult
	}

	pids := make([]int32, 0, len(args.PIDs))
	for _, pid64 := range args.PIDs {
		if pid64 < 0 || pid64 > math.MaxInt32 {
			return xconn.NewInvocationError(common.ErrInvalidArgument, "pid out of range")
		}
		pids = append(pids, int32(pid64))
	}

	results, err := SignalProcesses(pids, args.Signal)
	if err != nil {
		return xconn.NewInvocationError(common.ErrInvalidArgument, err.Error())
	}
	return encryptResult(enc, results)
}

func (d *Deskconn) handleAppList(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
	enc, errResult := d.callerKeys(inv)
	if errResult != nil {
		return errResult
	}

	procs, err := d.processes.List()
	if err != nil {
		return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
	}
	return encryptResult(enc, d.appRegistry.Build(procs))
}

// imageResult is an image as the encrypted handlers return it; Data is base64 in JSON.
type imageResult struct {
	Mime string `json:"mime"`
	Data []byte `json:"data"`
}

func (d *Deskconn) handleAppIcon(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
	enc, errResult := d.callerKeys(inv)
	if errResult != nil {
		return errResult
	}

	var name string
	if errResult = decryptArgs(enc, inv, &name); errResult != nil {
		return errResult
	}

	mimeType, data, err := ReadIcon(name)
	if err != nil {
		return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
	}
	return encryptResult(enc, imageResult{Mime: mimeType, Data: data})
}
