package handlers

import (
	"context"
	"strings"
	"time"
)

const timeoutWarpPageStatus = 4 * time.Second

// Page reads are independent of connection-control deadlines. An unsuccessful
// command must carry an error even when it produced output, so the page keeps
// its last valid WARP state instead of treating a failed read as disconnected.
func probeWarpPageStatus(ctx context.Context, run func(context.Context, string, ...string) (string, error)) warpSnapshot {
	ctx, cancel := context.WithTimeout(ctx, timeoutWarpPageStatus)
	defer cancel()

	out, err := run(ctx, "warp-cli", "--json", "status")
	result := parseWarpStatus(out)
	switch {
	case ctx.Err() != nil:
		result.Error = ctx.Err().Error()
	case err != nil:
		result.Error = err.Error()
	case strings.TrimSpace(result.Status) == "":
		result.Error = "WARP 状态查询未返回有效状态"
	}
	return result
}
