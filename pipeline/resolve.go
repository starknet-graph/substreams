package pipeline

import (
	"fmt"

	"github.com/streamingfast/bstream"
	pbsubstreams "github.com/streamingfast/substreams/pb/sf/substreams/v1"
	"github.com/streamingfast/substreams/reqctx"
	grpccodes "google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type getRecentFinalBlockFunc func() (uint64, error)

func BuildRequestDetails(request *pbsubstreams.Request, isSubRequest bool, getRecentFinalBlock getRecentFinalBlockFunc) (req *reqctx.RequestDetails, err error) {
	req = &reqctx.RequestDetails{
		Request:        request,
		IsSubRequest:   isSubRequest,
		StopBlockNum:   request.StopBlockNum,
		IsOutputModule: map[string]bool{},
	}
	for _, modName := range request.OutputModules {
		req.IsOutputModule[modName] = true
	}

	// FIXME:
	// CURSOR: if cursor is on a forked block, we NEED to kick off the LIVE
	//         process directly, even if that's realllly in the past.
	///        Eventually, we have a first process that corrects the live segment
	///        joining on a final segment, and then kick off parallel processing
	///        until a new, more recent, live block.
	// See also `resolveStartBlockNum`'s TODO
	req.RequestStartBlockNum, err = resolveStartBlockNum(request)
	if err != nil {
		return nil, err
	}

	if request.ProductionMode {
		if maxHandoff, err := getRecentFinalBlock(); err != nil {
			if request.StopBlockNum == 0 {
				return nil, fmt.Errorf("cannot determine a recent finalized block: %w", err)
			}
			req.LinearHandoffBlockNum = request.StopBlockNum
		} else {
			if request.StopBlockNum == 0 {
				req.LinearHandoffBlockNum = maxHandoff
			} else {
				req.LinearHandoffBlockNum = minOf(request.StopBlockNum, maxHandoff)
			}
		}
	} else {
		if maxHandoff, err := getRecentFinalBlock(); err != nil {
			req.LinearHandoffBlockNum = req.RequestStartBlockNum
		} else {
			req.LinearHandoffBlockNum = minOf(req.RequestStartBlockNum, maxHandoff)
		}
	}

	return req, nil
}

func resolveStartBlockNum(req *pbsubstreams.Request) (uint64, error) {
	// TODO(abourget): a caller will need to verify that, if there's a cursor.Step that is New or Undo,
	// then we need to validate that we are returning not only a number, but an ID,
	// We then need to sync from a known finalized Snapshot's block, down to the potentially
	// forked block in the Cursor, to then send the Substreams Undo payloads to the user,
	// before continuing on to live (or parallel download, if the fork happened way in the past
	// and everything is irreversible.

	if req.StartBlockNum < 0 {
		return 0, status.Error(grpccodes.InvalidArgument, "start block num must be positive")
	}

	if req.StartCursor == "" {
		return uint64(req.StartBlockNum), nil
	}

	cursor, err := bstream.CursorFromOpaque(req.StartCursor)
	if err != nil {
		return 0, status.Errorf(grpccodes.InvalidArgument, "invalid start cursor %q: %s", cursor, err.Error())
	}
	if cursor.Step.Matches(bstream.StepIrreversible) {
		return cursor.Block.Num() + 1, nil // this block was the last sent to the customer
	}
	if cursor.Step.Matches(bstream.StepNew) {
		return cursor.Block.Num() + 1, nil // this block was the last sent to the customer
	}
	if cursor.Step.Matches(bstream.StepUndo) {
		return cursor.Block.Num(), nil
	}
	return 0, fmt.Errorf("invalid start cursor step")
}

func minOf(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return b
}
