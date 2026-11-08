package deskconn

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"

	"github.com/xconnio/deskconn/internal/common"
)

// StreamClientKeyExchange derives end-to-end keys before feature messages.
func StreamClientKeyExchange(stream net.Conn) (sendKey, receiveKey []byte, err error) {
	publicKey, privateKey, err := common.CreateX25519KeyPair()
	if err != nil {
		return nil, nil, err
	}
	if err := common.WriteMsg(stream, common.KeyExchangeMsg{PublicKey: publicKey}); err != nil {
		return nil, nil, err
	}

	frame, err := common.ReadFrame(stream)
	if err != nil {
		return nil, nil, err
	}

	var peerKey common.KeyExchangeMsg
	if err := json.Unmarshal(frame, &peerKey); err != nil {
		return nil, nil, err
	}
	if len(peerKey.PublicKey) != 32 {
		return nil, nil, fmt.Errorf("invalid peer public key length: %d", len(peerKey.PublicKey))
	}

	return common.ClientKeyExchangeKeys(privateKey, peerKey.PublicKey)
}

func streamRequest(sess common.StreamSession, req common.FSRequest) (*common.FSResponse, error) {
	procedure, input := common.ProcedureFileListStream, false
	if req.Op == common.FSOpInit {
		procedure, input = common.ProcedureFileInitStream, true
	}
	stream, err := common.OpenRPCStream(context.Background(), sess, procedure, input)
	if err != nil {
		return nil, err
	}
	defer stream.Close()

	sendKey, receiveKey, err := StreamClientKeyExchange(stream)
	if err != nil {
		return nil, err
	}

	if err := common.WriteEncryptedMsg(stream, req, sendKey); err != nil {
		return nil, err
	}

	var resp common.FSResponse
	if err := common.ReadEncryptedMsg(stream, &resp, receiveKey); err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, responseErr(resp)
	}
	return &resp, nil
}

// streamReadWorker opens one stream on sess (the same connection shared by
// every parallel worker and the control call) and owns it for the lifetime
// of the goroutine running it, reusing it across every job pulled from
// jobs.
func streamReadWorker(sess common.StreamSession, rootArg string, jobs <-chan transferChunk, localPath string,
	localIsDir bool, sourceRoot string, progress *common.TransferProgress) error {
	stream, err := common.OpenRPCStream(context.Background(), sess, common.ProcedureFileReadStream, false)
	if err != nil {
		return err
	}
	defer stream.Close()

	sendKey, receiveKey, err := StreamClientKeyExchange(stream)
	if err != nil {
		return err
	}

	for chunk := range jobs {
		if err := quicReadOneChunk(stream, sendKey, receiveKey, rootArg, chunk, localPath, localIsDir, sourceRoot,
			progress); err != nil {
			return err
		}
	}
	return nil
}

func quicReadOneChunk(stream net.Conn, sendKey, receiveKey []byte, rootArg string, chunk transferChunk,
	localPath string, localIsDir bool, sourceRoot string, progress *common.TransferProgress) error {
	req := common.FSRequest{Op: common.FSOpRead, Path: rootArg, RelPath: chunk.RelPath, Offset: chunk.Offset,
		Length: chunk.Length}
	if err := common.WriteEncryptedMsg(stream, req, sendKey); err != nil {
		return err
	}

	var resp common.FSResponse
	if err := common.ReadEncryptedMsg(stream, &resp, receiveKey); err != nil {
		return err
	}
	if !resp.OK {
		return responseErr(resp)
	}

	dest := common.ResolveDestPath(localPath, localIsDir, sourceRoot, chunk.RelPath)
	f, err := os.OpenFile(dest, os.O_WRONLY, 0) //nolint:gosec
	if err != nil {
		return err
	}
	defer f.Close()

	ow := io.NewOffsetWriter(f, chunk.Offset)
	return common.CopyDecrypted(ow, stream, chunk.Length, receiveKey, progress)
}

// streamWriteWorker is the upload counterpart to streamReadWorker: it opens one
// stream on the shared sess and owns it for the lifetime of the goroutine
// running it, reusing it across every job pulled from jobs.
func streamWriteWorker(sess common.StreamSession, rootArg string, jobs <-chan transferChunk, localBase string,
	sourceIsDir, targetIsDirHint bool, progress *common.TransferProgress) error {
	stream, err := common.OpenRPCStream(context.Background(), sess, common.ProcedureFileWriteStream, true)
	if err != nil {
		return err
	}
	defer stream.Close()

	sendKey, receiveKey, err := StreamClientKeyExchange(stream)
	if err != nil {
		return err
	}

	for chunk := range jobs {
		if err := quicWriteOneChunk(stream, sendKey, receiveKey, rootArg, chunk, localBase, sourceIsDir,
			targetIsDirHint, progress); err != nil {
			return err
		}
	}
	return nil
}

func quicWriteOneChunk(stream net.Conn, sendKey, receiveKey []byte, rootArg string, chunk transferChunk,
	localBase string, sourceIsDir, targetIsDirHint bool, progress *common.TransferProgress) error {
	req := common.FSRequest{
		Op: common.FSOpWrite, Path: rootArg, RelPath: chunk.RelPath, Offset: chunk.Offset, Length: chunk.Length,
		SourceIsDir: sourceIsDir, TargetIsDirHint: targetIsDirHint,
	}
	if err := common.WriteEncryptedMsg(stream, req, sendKey); err != nil {
		return err
	}

	var ack common.FSResponse
	if err := common.ReadEncryptedMsg(stream, &ack, receiveKey); err != nil {
		return err
	}
	if !ack.OK {
		return responseErr(ack)
	}

	absLocal := filepath.Join(localBase, filepath.FromSlash(chunk.RelPath))
	f, err := os.Open(absLocal) //nolint:gosec
	if err != nil {
		return err
	}
	defer f.Close()

	section := io.NewSectionReader(f, chunk.Offset, chunk.Length)
	if err := common.CopyEncrypted(stream, section, chunk.Length, sendKey, progress); err != nil {
		return err
	}

	var final common.FSResponse
	if err := common.ReadEncryptedMsg(stream, &final, receiveKey); err != nil {
		return err
	}
	if !final.OK {
		return responseErr(final)
	}
	return nil
}

// DownloadFiles downloads a file or directory using parallel encrypted RPC streams.
// A nonpositive worker count selects the default.
func DownloadFiles(sess common.StreamSession, remotePath, localPath string,
	recursive bool, numWorkers int) error {
	return downloadFiles(remotePath, localPath, recursive, numWorkers,
		func(req common.FSRequest) (*common.FSResponse, error) { return streamRequest(sess, req) },
		func(sourceRoot string, localIsDir bool, progress *common.TransferProgress) func(jobs <-chan transferChunk) error {
			return func(jobs <-chan transferChunk) error {
				return streamReadWorker(sess, remotePath, jobs, localPath, localIsDir, sourceRoot, progress)
			}
		},
	)
}

// UploadFiles uploads a file or directory using parallel encrypted RPC streams.
// A nonpositive worker count selects the default.
func UploadFiles(sess common.StreamSession, localPath, remotePath string,
	recursive bool, numWorkers int) error {
	localBase := filepath.Dir(localPath)
	return uploadFiles(localPath, remotePath, recursive, numWorkers,
		func(req common.FSRequest) (*common.FSResponse, error) { return streamRequest(sess, req) },
		func(sourceIsDir, targetIsDirHint bool, progress *common.TransferProgress) func(jobs <-chan transferChunk) error {
			return func(jobs <-chan transferChunk) error {
				return streamWriteWorker(sess, remotePath, jobs, localBase, sourceIsDir, targetIsDirHint,
					progress)
			}
		},
	)
}

func responseErr(resp common.FSResponse) error {
	if resp.Err == "" {
		return fmt.Errorf("remote operation failed")
	}
	return fmt.Errorf("%s", resp.Err) //nolint:err113
}
