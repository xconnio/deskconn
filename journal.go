//nolint:godot
package deskconn

// #cgo LDFLAGS: -l:libsystemd.so.0
// #include <stdlib.h>
// #include <stdint.h>
// #include <string.h>
//
// typedef struct sd_journal sd_journal;
//
// extern int sd_journal_open(sd_journal **ret, int flags);
// extern void sd_journal_close(sd_journal *j);
// extern int sd_journal_next(sd_journal *j);
// extern int sd_journal_seek_tail(sd_journal *j);
// extern int sd_journal_previous_skip(sd_journal *j, uint64_t skip);
// extern int sd_journal_wait(sd_journal *j, uint64_t timeout_usec);
// extern int sd_journal_enumerate_data(sd_journal *j, const void **data, size_t *l);
// extern void sd_journal_restart_data(sd_journal *j);
// extern int sd_journal_add_match(sd_journal *j, const void *data, size_t size);
// extern int sd_journal_seek_realtime_usec(sd_journal *j, uint64_t usec);
// extern int sd_journal_get_realtime_usec(sd_journal *j, uint64_t *ret);
//
// static int journal_open(sd_journal **j) {
//     return sd_journal_open(j, 0);
// }
//
// static void journal_close(sd_journal *j) {
//     sd_journal_close(j);
// }
//
// static int journal_next(sd_journal *j) {
//     return sd_journal_next(j);
// }
//
// static int journal_seek_tail(sd_journal *j) {
//     return sd_journal_seek_tail(j);
// }
//
// static int journal_previous_skip(sd_journal *j, uint64_t skip) {
//     return sd_journal_previous_skip(j, skip);
// }
//
// static int journal_wait(sd_journal *j, uint64_t usec) {
//     return sd_journal_wait(j, usec);
// }
//
// static int journal_add_match(sd_journal *j, const char *s) {
//     return sd_journal_add_match(j, s, strlen(s));
// }
//
// static int journal_seek_realtime_usec(sd_journal *j, uint64_t usec) {
//     return sd_journal_seek_realtime_usec(j, usec);
// }
//
// static uint64_t journal_get_realtime_usec(sd_journal *j) {
//     uint64_t usec = 0;
//     sd_journal_get_realtime_usec(j, &usec);
//     return usec;
// }
//
// // Returns the next field as a newly-malloc'd C string, or NULL when done.
// // The caller must free() the returned pointer.
// static char* journal_next_field(sd_journal *j) {
//     const void *data;
//     size_t l;
//     if (sd_journal_enumerate_data(j, &data, &l) <= 0)
//         return NULL;
//     char *s = (char*)malloc(l + 1);
//     if (!s) return NULL;
//     memcpy(s, data, l);
//     s[l] = '\0';
//     return s;
// }
//
// static void journal_restart_data(sd_journal *j) {
//     sd_journal_restart_data(j);
// }
import "C"

import (
	"fmt"
	"strings"
	"time"
	"unsafe"
)

type journal struct {
	j *C.sd_journal
}

func openJournal() (*journal, error) {
	var j *C.sd_journal
	if ret := C.journal_open(&j); ret < 0 {
		return nil, fmt.Errorf("sd_journal_open failed: %d", ret)
	}
	return &journal{j: j}, nil
}

func (j *journal) close() {
	C.journal_close(j.j)
}

func (j *journal) next() (int, error) {
	ret := C.journal_next(j.j)
	if ret < 0 {
		return 0, fmt.Errorf("sd_journal_next: %d", ret)
	}
	return int(ret), nil
}

func (j *journal) seekTail() error {
	if ret := C.journal_seek_tail(j.j); ret < 0 {
		return fmt.Errorf("sd_journal_seek_tail: %d", ret)
	}
	return nil
}

func (j *journal) previousSkip(n uint64) {
	C.journal_previous_skip(j.j, C.uint64_t(n))
}

// wait blocks up to the given duration for new entries. Returns true if new entries arrived.
func (j *journal) wait(d time.Duration) bool {
	usec := C.uint64_t(d.Microseconds())
	ret := C.journal_wait(j.j, usec)
	return ret > 0
}

func (j *journal) addMatch(match string) error {
	cs := C.CString(match)
	defer C.free(unsafe.Pointer(cs))
	if ret := C.journal_add_match(j.j, cs); ret < 0 {
		return fmt.Errorf("sd_journal_add_match: %d", ret)
	}
	return nil
}

func (j *journal) seekRealtimeUsec(usec uint64) error {
	if ret := C.journal_seek_realtime_usec(j.j, C.uint64_t(usec)); ret < 0 {
		return fmt.Errorf("sd_journal_seek_realtime_usec: %d", ret)
	}
	return nil
}

func (j *journal) realtimeUsec() uint64 {
	return uint64(C.journal_get_realtime_usec(j.j))
}

// fields returns all KEY=VALUE pairs for the current entry.
func (j *journal) fields() map[string]string {
	C.journal_restart_data(j.j)
	result := make(map[string]string)
	for {
		cs := C.journal_next_field(j.j)
		if cs == nil {
			break
		}
		s := C.GoString(cs)
		C.free(unsafe.Pointer(cs))
		if idx := strings.IndexByte(s, '='); idx > 0 {
			result[s[:idx]] = s[idx+1:]
		}
	}
	return result
}

func formatEntry(tsUsec uint64, fields map[string]string) string {
	ts := time.UnixMicro(int64(tsUsec)).Local()
	hostname := fields["_HOSTNAME"]
	ident := fields["SYSLOG_IDENTIFIER"]
	if ident == "" {
		ident = fields["_COMM"]
	}
	pid := fields["_PID"]
	msg := fields["MESSAGE"]

	tsStr := ts.Format("Jan 02 15:04:05.000000")
	if pid != "" {
		return fmt.Sprintf("%s %s %s[%s]: %s\n", tsStr, hostname, ident, pid, msg)
	}
	if ident != "" {
		return fmt.Sprintf("%s %s %s: %s\n", tsStr, hostname, ident, msg)
	}
	return fmt.Sprintf("%s %s: %s\n", tsStr, hostname, msg)
}
