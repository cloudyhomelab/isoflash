// SPDX-FileCopyrightText: 2026 Sujoy Das
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

const (
	// Bytes per write call.
	chunkSize = 4 << 20
	// Flush to the device after this many bytes, so progress tracks the drive
	// rather than the page cache.
	syncEvery = 64 << 20
	// Minimum time between progress reports to the UI.
	progressInterval = 200 * time.Millisecond
)

var errAborted = errors.New("aborted")

type progressMsg struct {
	written  int64
	flushing bool
}

type writeDoneMsg struct {
	written int64
	err     error
}

// writeImage copies isoPath to devPath, reporting on out, then closes out.
func writeImage(ctx context.Context, isoPath, devPath string, out chan<- tea.Msg) {
	written, err := copyImage(ctx, isoPath, devPath, out)
	out <- writeDoneMsg{written: written, err: err}
	close(out)
}

func copyImage(ctx context.Context, isoPath, devPath string, out chan<- tea.Msg) (int64, error) {
	src, err := os.Open(isoPath)
	if err != nil {
		return 0, err
	}
	defer src.Close()

	// No O_CREATE: if the drive was unplugged, opening must fail instead of
	// creating a regular file in /dev. O_EXCL on a block device fails with
	// EBUSY while the kernel has it, or any of its partitions, mounted.
	dst, err := os.OpenFile(devPath, os.O_WRONLY|os.O_EXCL, 0)
	if err != nil {
		return 0, err
	}
	defer dst.Close()
	info, err := dst.Stat()
	if err != nil {
		return 0, err
	}
	if info.Mode()&os.ModeDevice == 0 || info.Mode()&os.ModeCharDevice != 0 {
		return 0, fmt.Errorf("%s is not a block device", devPath)
	}

	buf := make([]byte, chunkSize)
	var written, unsynced int64
	var lastReport time.Time
	for {
		if ctx.Err() != nil {
			return written, errAborted
		}
		n, readErr := io.ReadFull(src, buf)
		if n > 0 {
			if _, err := dst.Write(buf[:n]); err != nil {
				return written, err
			}
			written += int64(n)
			unsynced += int64(n)
			if unsynced >= syncEvery {
				if err := dst.Sync(); err != nil {
					return written, err
				}
				unsynced = 0
			}
			if time.Since(lastReport) >= progressInterval {
				out <- progressMsg{written: written}
				lastReport = time.Now()
			}
		}
		if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
			break
		}
		if readErr != nil {
			return written, readErr
		}
	}

	out <- progressMsg{written: written, flushing: true}
	if err := dst.Sync(); err != nil {
		return written, err
	}
	return written, dst.Close()
}
