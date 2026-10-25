package connectivity

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"

	api "github.com/xconnio/xconn-core/bindings/go"
)

type RelayKind string

const (
	RelayKindStream   RelayKind = "stream"
	RelayKindWebRTC   RelayKind = "webrtc"
	maxRelayFrameSize           = 1 << 20
)

type RelayHeader struct {
	Kind  RelayKind `json:"kind"`
	Label string    `json:"label,omitempty"`
}

func ReadRelayHeader(reader io.Reader) (RelayHeader, error) {
	var header RelayHeader
	data, err := readFrame(reader)
	if err == nil {
		err = json.Unmarshal(data, &header)
	}
	return header, err
}

func writeHeader(writer io.Writer, header RelayHeader) error {
	data, err := json.Marshal(header)
	if err != nil {
		return err
	}
	return writeFrame(writer, data)
}

func readFrame(reader io.Reader) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return nil, err
	}
	size := binary.BigEndian.Uint32(header[:])
	if size > maxRelayFrameSize {
		return nil, fmt.Errorf("relay frame exceeds 1 MiB")
	}
	data := make([]byte, size)
	_, err := io.ReadFull(reader, data)
	return data, err
}

func writeFrame(writer io.Writer, data []byte) error {
	if len(data) > maxRelayFrameSize {
		return fmt.Errorf("relay frame exceeds 1 MiB")
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(data))) //nolint:gosec // Length is bounded to 1 MiB above.
	if _, err := writer.Write(header[:]); err != nil {
		return err
	}
	_, err := writer.Write(data)
	return err
}

func ReadRelayFrame(reader io.Reader) ([]byte, bool, error) {
	var kind [1]byte
	if _, err := io.ReadFull(reader, kind[:]); err != nil {
		return nil, false, err
	}
	if kind[0] > 1 {
		return nil, false, fmt.Errorf("invalid relay frame kind")
	}
	data, err := readFrame(reader)
	return data, kind[0] == 1, err
}

func WriteRelayFrame(writer io.Writer, data []byte, text bool) error {
	kind := byte(0)
	if text {
		kind = 1
	}
	if _, err := writer.Write([]byte{kind}); err != nil {
		return err
	}
	return writeFrame(writer, data)
}

func relayStream(ctx context.Context, stream io.ReadWriteCloser, path string) {
	defer stream.Close()
	var dialer net.Dialer
	local, err := dialer.DialContext(ctx, "unix", path)
	if err != nil {
		return
	}
	defer local.Close()
	stop := context.AfterFunc(ctx, func() { _ = stream.Close(); _ = local.Close() })
	defer stop()
	if err := writeHeader(local, RelayHeader{Kind: RelayKindStream}); err != nil {
		return
	}
	finished := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(local, stream); finished <- struct{}{} }()
	go func() { _, _ = io.Copy(stream, local); finished <- struct{}{} }()
	<-finished
	_ = local.Close()
	_ = stream.Close()
	<-finished
}

func relayDataChannel(ctx context.Context, channel *api.WebRTCDataChannel, path string) {
	defer channel.Close()
	var dialer net.Dialer
	local, err := dialer.DialContext(ctx, "unix", path)
	if err != nil {
		return
	}
	defer local.Close()
	stop := context.AfterFunc(ctx, func() { _ = local.Close(); _ = channel.Close() })
	defer stop()
	if err := writeHeader(local, RelayHeader{Kind: RelayKindWebRTC, Label: channel.Label()}); err != nil {
		return
	}

	finished := make(chan struct{}, 2)
	go func() {
		defer func() { finished <- struct{}{} }()
		for {
			packet, err := channel.ReadDataPacketContext(ctx)
			if err != nil {
				return
			}
			if err = WriteRelayFrame(local, packet.Data, packet.IsString); err != nil {
				return
			}
		}
	}()
	go func() {
		defer func() { finished <- struct{}{} }()
		for {
			data, text, err := ReadRelayFrame(local)
			if err != nil {
				return
			}
			if text {
				err = channel.SendText(string(data))
			} else {
				err = channel.Send(data)
			}
			if err != nil {
				return
			}
		}
	}()
	<-finished
	_ = channel.Close()
	_ = local.Close()
	<-finished
}
