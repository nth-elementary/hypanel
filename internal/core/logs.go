package core

import (
	"bytes"
	"fmt"
	"io"
	"os"
)

// MaxVacuumKeep is the largest size a vacuum request can ask to keep. Keeping is done in
// memory, and nobody looking to shrink a log needs more than this of it.
const MaxVacuumKeep = 1 << 30

// TrimLog cuts a log down to its most recent keep bytes, on a line boundary, and returns
// how many bytes it freed. It rewrites the file in place rather than replacing it, because
// nginx and docker hold the file open and would keep writing to a replaced one.
func TrimLog(path string, keep int64) (int64, error) {
	if keep < 0 || keep > MaxVacuumKeep {
		return 0, fmt.Errorf("invalid size to keep")
	}
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	size := info.Size()
	if size <= keep {
		return 0, nil
	}
	var tail []byte
	if keep > 0 {
		in, err := os.Open(path)
		if err != nil {
			return 0, err
		}
		defer in.Close()
		if _, err := in.Seek(size-keep, io.SeekStart); err != nil {
			return 0, err
		}
		tail = make([]byte, keep)
		n, err := io.ReadFull(in, tail)
		if err != nil && err != io.ErrUnexpectedEOF {
			return 0, err
		}
		tail = tail[:n]
		// The cut probably landed mid-line; drop the partial first line.
		if i := bytes.IndexByte(tail, '\n'); i >= 0 {
			tail = tail[i+1:]
		}
	}
	out, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return 0, err
	}
	defer out.Close()
	if _, err := out.Write(tail); err != nil {
		return 0, err
	}
	return size - int64(len(tail)), nil
}
