package transfer

const (
	MaxManifestBytes                 = 1 << 20
	MaxRecordLineBytes               = 1 << 20
	MaxRecordLines                   = 5_000_000
	RestartSegmentRecords            = 100_000
	MaxBlobBytes               int64 = 2 << 30
	MaxBlobs                         = 1_000_000
	MaxExpandedBytes           int64 = 200 << 30
	MaxSourceFieldsBytes             = 64 << 10
	MaxBodyTextBytes                 = 512 << 10
	MaxParticipantsPerRecord         = 512
	MaxAttachmentsPerRecord          = 512
	MaxDatesPerRecord                = 8
	MaxJSONDepth                     = 32
	MaxPackagePathSegmentBytes       = 64
	MaxPackagePathBytes              = 400
	MaxInlineFindings                = 250
	MaxDiagnosticSpoolBytes          = 64 << 20

	MaxArchiveIDBytes     = 128
	MaxPersonUIDBytes     = 256
	MaxPersonHintBytes    = 512
	MaxNameBytes          = 200
	MaxContactPoints      = 200
	MaxContactPointBytes  = 200
	MaxSourceTypeRawBytes = 64
	MaxSourceRefBytes     = 512
	MaxRecordRefBytes     = 512
	MaxContinuationBytes  = 4096
	MaxZipEntries         = MaxBlobs + 260
	MaxChecksumBytes      = 128 << 20
	MaxZipDirectoryBytes  = 512 << 20

	MaxCallDurationMilliseconds int64 = 31 * 24 * 60 * 60 * 1000
	MaxRecurrenceRules                = 256
	MaxRecurrenceLineBytes            = 4096
	MaxSpeakerLabels                  = 512
)
