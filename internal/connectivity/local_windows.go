package connectivity

import (
	"context"
	"errors"
	"os"

	"golang.org/x/sys/windows"

	api "github.com/xconnio/xconn-core/bindings/go"
)

func listenAppLayer(router *api.Router, path string) (*api.Listener, error) {
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	err = windows.LockFileEx(windows.Handle(lock.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	listener, err := listenLoopbackApp(router, path)
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	return api.NewListener(&lockedAppListener{Listener: listener, lock: lock}, listener.Addr()), nil
}

// ConnectApp joins the local daemon using its per-user endpoint credentials.
func ConnectApp(ctx context.Context, path, realm string) (*api.Session, error) {
	return connectLoopbackApp(ctx, path, realm)
}

func secureLocalEndpoint(file *os.File) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;" + user.User.Sid.String() + ")")
	if err != nil {
		return err
	}
	acl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(file.Name(), windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil)
}

// Holding the file lock prevents two daemons from replacing the same endpoint.
// Windows releases it when the process exits, including after a crash.
type lockedAppListener struct {
	*api.Listener
	lock *os.File
}

func (l *lockedAppListener) Close() error {
	return errors.Join(l.Listener.Close(), l.lock.Close())
}
