// +build windows

/*
 * Copyright (c) 2019. Abstrium SAS <team (at) pydio.com>
 * This file is part of Pydio Cells.
 *
 * Pydio Cells is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * Pydio Cells is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with Pydio Cells.  If not, see <http://www.gnu.org/licenses/>.
 *
 * The latest code can be found at <https://pydio.com>.
 */

package control

import (
	"context"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/pydio/cells/common/log"
	"github.com/pydio/cells/common/proto/tree"
)

func browseWinVolumes(ctx context.Context) (children []*tree.Node) {

	h := syscall.MustLoadDLL("kernel32.dll")
	doneChan := make(chan string, 1)

	for _, drive := range "ABCDEFGHIJKLMNOPQRSTUVWXYZ" {
		go func() {
			driveLetter := string(drive) + ":"
			_, err := os.Open(driveLetter)
			if err == nil {
				doneChan <- ""
			}
		}()

		select {
		case <-doneChan:
			c := h.MustFindProc("GetDiskFreeSpaceExW")
			var freeBytes uint64
			rootDrive := string(drive) + ":"
			_, _, _ = c.Call(uintptr(unsafe.Pointer(syscall.StringToUTF16Ptr(rootDrive))), uintptr(unsafe.Pointer(&freeBytes)), 0, 0)

			log.Logger(ctx).Info("adding volume " + strings.ToUpper(string(drive)))
			children = append(children, &tree.Node{
				Path: fmt.Sprintf("/%c:", drive),
				Size: int64(freeBytes),
				Type: tree.NodeType_COLLECTION,
				Uuid: fmt.Sprintf("%c-drive", drive),
			})
		case <-time.After(time.Millisecond * 10):
			// log.Logger(ctx).Error("Volume" + string(drive) + " listing took too long.")
		}
	}

	return
}
