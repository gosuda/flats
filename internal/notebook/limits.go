package notebook

import (
	"fmt"
	"math"
	"time"
)

const (
	MaxPages                 = 2000
	MaxBlocksPerPage         = 2000
	MaxRichTextCodePoints    = 8000
	MaxCodeCodePoints        = 8000
	MaxAltCodePoints         = 8000
	MaxTitleCodePoints       = 8000
	MaxCellCodePoints        = 8000
	MaxNameCodePoints        = 200
	MaxURLBytes              = 2048
	MaxDatabases             = 200
	MaxRowsPerDatabase       = 5000
	MaxPropertiesPerDatabase = 32
	MaxOptionsPerProperty    = 100
	MaxViewsPerDatabase      = 10
	MaxDatabaseBytes         = 64 << 20
	MaxPageBytes             = 64 << 20
	MaxPageDepth             = 8
	MaxBlockDepth            = 6
	MaxOperations            = 500
	MaxActionBytes           = 8 << 20
	MaxActionParts           = 32
	ActionAssemblyTimeout    = 30 * time.Second
	MaxAgentOperationBytes   = 8 << 20
	MaxImagePixels           = 24000000
	MaxImageSide             = 16384
	MaxGIFFrames             = 500
	MaxGIFPixels             = 200000000
	MaxSpillBytes            = 1 << 30
	SpillIdleTimeout         = 60 * time.Second
	SpillProgressInterval    = 10 * time.Second
	MinCaptureProgressBytes  = 1 << 20
	MaxAgentReads            = 4
	MaxClientFrameBytes      = 256 << 10
	MaxServerFrameBytes      = 512 << 10
	DefaultReadRecords       = 500
	MaxReadRecords           = 2000
	TargetReadBytes          = 1 << 20
	MaxPositionBytes         = 32
	SessionIdleTimeout       = 24 * time.Hour
	CommandRetention         = 24 * time.Hour
	RevisionIdleTimeout      = 30 * time.Second
	RevisionMaxAge           = 5 * time.Minute
	MaintenanceInterval      = 5 * time.Second
	DefaultRevisionQuota     = 100
	DefaultPinQuota          = 20
	MaxUndoEntries           = 100
	MaxUndoBytes             = 32 << 20
	MaxDeltaEntries          = 20000
	MaxDeltaAge              = time.Hour
	MaxDeltaBytes            = 16 << 20
	MaxOutcomeBytes          = 64 << 10
	MaxReceiptBytes          = 16 << 20
	MaxDeletionBytes         = 16 << 20
	MaxPublicFrameBytes      = 256 << 20
	MaxPrivateConnections    = 64
	MaxPublicConnections     = 256
	MaxPrivateQueueBytes     = 4 << 20
	MaxPublicQueueBytes      = 1 << 20
	MaxAssemblyBytes         = 64 << 20
	DatabaseWindowRows       = 1000
)

// CheckAdmission compares one value or owned scope's measured usage against its
// action-start usage. Call it separately for every applicable content limit;
// unused capacity in another value or scope cannot offset growing excess.
// A fresh value has before == 0. Rollback bypasses content admission entirely.
// Codecs validate structure only and must not call this function.
func CheckAdmission(before, after, limit int64) error {
	if before < 0 || after < 0 || limit < 0 {
		return fmt.Errorf("negative content usage or limit")
	}
	if after > limit && after > before {
		return fmt.Errorf("content usage grows beyond limit %d: %d -> %d", limit, before, after)
	}
	return nil
}

func MaxUpdateBodyBytes(uploadMaxBytes int64) (int64, error) {
	if uploadMaxBytes < 0 {
		return 0, fmt.Errorf("negative upload limit")
	}
	groups := uploadMaxBytes / 3
	if uploadMaxBytes%3 != 0 {
		groups++
	}
	if groups > (math.MaxInt64-MaxAgentOperationBytes)/4 {
		return 0, fmt.Errorf("update body limit overflows int64")
	}
	return MaxAgentOperationBytes + 4*groups, nil
}
