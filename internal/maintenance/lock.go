package maintenance

import (
	"errors"
	"os"
	"path/filepath"

	"syscall"
)

var ErrBusy = errors.New("database maintenance is already in progress")

// Lease keeps the persistent lock inode open until the operation finishes.
// Gateway and backup use shared leases; replacement and ownership migration
// require an exclusive lease after those writers have stopped.
type Lease struct{ file *os.File }

func AcquireShared(dir string) (*Lease, error)    { return acquire(dir, syscall.LOCK_SH) }
func AcquireExclusive(dir string) (*Lease, error) { return acquire(dir, syscall.LOCK_EX) }

func acquire(dir string, mode int) (*Lease, error) {
	file, err := os.OpenFile(filepath.Join(dir, ".maintenance.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(file.Fd()), mode|syscall.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrBusy
		}
		return nil, err
	}
	return &Lease{file: file}, nil
}

func (l *Lease) Close() error { return l.file.Close() }
