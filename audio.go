package deskconn

import (
	"fmt"

	"github.com/jfreymuth/pulse"
	"github.com/jfreymuth/pulse/proto"
)

type Audio struct {
	client *pulse.Client
}

func NewAudio() *Audio {
	client, _ := pulse.NewClient()
	return &Audio{client: client}
}

func (a *Audio) Close() {
	if a.client != nil {
		a.client.Close()
	}
}

func (a *Audio) defaultSinkInfo() (*proto.GetSinkInfoReply, error) {
	if a == nil || a.client == nil {
		return nil, fmt.Errorf("audio unavailable")
	}
	var reply proto.GetSinkInfoReply
	if err := a.client.RawRequest(&proto.GetSinkInfo{SinkIndex: proto.Undefined}, &reply); err != nil {
		return nil, err
	}
	return &reply, nil
}

func (a *Audio) setMute(mute bool) error {
	sink, err := a.defaultSinkInfo()
	if err != nil {
		return err
	}
	return a.client.RawRequest(&proto.SetSinkMute{SinkIndex: sink.SinkIndex, Mute: mute}, nil)
}

func (a *Audio) Mute() error {
	return a.setMute(true)
}

func (a *Audio) Unmute() error {
	return a.setMute(false)
}

func (a *Audio) ToggleMute() (bool, error) {
	sink, err := a.defaultSinkInfo()
	if err != nil {
		return false, err
	}

	muted := !sink.Mute
	if err := a.client.RawRequest(&proto.SetSinkMute{SinkIndex: sink.SinkIndex, Mute: muted}, nil); err != nil {
		return false, err
	}

	return muted, nil
}

func (a *Audio) IsMuted() (bool, error) {
	sink, err := a.defaultSinkInfo()
	if err != nil {
		return false, err
	}
	return sink.Mute, nil
}
