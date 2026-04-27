package engine

import (
	"fmt"
	"log/slog"
)

// tmpSafetyMargin is reserved on top of the requested size when checking
// free space — covers FS journal overhead, the zip index sidecar written
// alongside the main archive, and small concurrent writers (logs, DB).
const tmpSafetyMargin int64 = 64 << 20 // 64 MiB

// diskAvailable is overridable in tests.
var diskAvailable = availableBytes

// ensureTmpSpace returns nil if dir's filesystem has at least
// need + tmpSafetyMargin bytes free. A Statfs failure is treated as
// non-fatal — we don't want a transient stat error to block the run; the
// underlying os.Create will surface a real ENOSPC if there genuinely is
// no space.
func ensureTmpSpace(dir string, need int64) error {
	avail, err := diskAvailable(dir)
	if err != nil {
		slog.Warn("free-space check failed, proceeding without guard", "dir", dir, "err", err)
		return nil
	}
	required := uint64(need) + uint64(tmpSafetyMargin)
	if avail < required {
		return fmt.Errorf("not enough space in %s: need %d bytes (+%d margin), have %d", dir, need, tmpSafetyMargin, avail)
	}
	return nil
}
