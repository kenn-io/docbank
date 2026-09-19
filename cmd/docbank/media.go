package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

var (
	mediaAcquire                                                 bool
	mediaCursor, mediaProfile, mediaInputID                      string
	mediaLimit                                                   int
	mediaFile, mediaReferenceFile, mediaOperationID              string
	mediaOccurrenceRef, mediaOccurrenceRevision, mediaPersonRef  string
	mediaSpeakerLabel, mediaSourceID, mediaOccurrenceID          string
	mediaArtifactKind, mediaOrigin, mediaProvider, mediaLanguage string
	mediaPlanFile                                                string
)

var mediaCmd = &cobra.Command{Use: "media", Short: "Retain and inspect recordings",
	Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() }}

var mediaSubmitCmd = &cobra.Command{Use: "submit", Short: "Retain supplied recording bytes or a private reference",
	Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if (mediaFile == "") == (mediaReferenceFile == "") {
			return usageError(errors.New("use exactly one of --file or --reference-file"))
		}
		if err := requireMediaMutationFlags(); err != nil {
			return err
		}
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		occurrence := mediaOccurrenceFlags(mediaSubmitFilename(mediaFile))
		if mediaReferenceFile != "" {
			reference, err := readPrivateReference(mediaReferenceFile)
			if err != nil {
				return err
			}
			receipt, err := c.SubmitRemoteRecording(cmd.Context(), api.MediaReferenceBody{
				OperationID: mediaOperationID, ReferenceURL: reference, Acquire: mediaAcquire,
				Occurrence: occurrence, Processing: mediaProcessingFlags()})
			if err != nil {
				return err
			}
			return writeCLIJSON(cmd.OutOrStdout(), receipt)
		}
		file, metadata, err := openMediaUpload(mediaFile)
		if err != nil {
			return err
		}
		defer func() { _ = file.Close() }()
		metadata.OperationID, metadata.Occurrence, metadata.Processing = mediaOperationID, occurrence, mediaProcessingFlags()
		receipt, err := c.SubmitSuppliedMedia(cmd.Context(), metadata, file)
		if err != nil {
			return err
		}
		return writeCLIJSON(cmd.OutOrStdout(), receipt)
	}}

var mediaListCmd = &cobra.Command{Use: "list", Short: "List caller-visible recording sources",
	Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		page, err := c.MediaSources(cmd.Context(), mediaCursor, mediaLimit)
		if err != nil {
			return err
		}
		return writeCLIJSON(cmd.OutOrStdout(), page)
	}}

var mediaStatusCmd = &cobra.Command{Use: "status <source-id>", Short: "Show current recording status",
	Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		receipt, err := c.MediaStatus(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		return writeCLIJSON(cmd.OutOrStdout(), receipt)
	}}

var mediaRetryCmd = &cobra.Command{Use: "retry <source-id>", Short: "Retry explicitly authorized transcript processing",
	Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if mediaOperationID == "" || mediaProfile == "" {
			return usageError(errors.New("--operation-id and --processing-profile are required"))
		}
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		receipt, err := c.RetryMedia(cmd.Context(), args[0], api.MediaRetryBody{OperationID: mediaOperationID,
			Processing: &api.MediaProcessingBody{Profile: mediaProfile, SuppliedInputID: mediaInputID}})
		if err != nil {
			return err
		}
		return writeCLIJSON(cmd.OutOrStdout(), receipt)
	}}

var mediaImportCmd = &cobra.Command{Use: "import-artifact <source-id>", Short: "Retain a bounded original input artifact",
	Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if mediaOperationID == "" || mediaFile == "" || mediaOccurrenceID == "" || mediaArtifactKind == "" {
			return usageError(errors.New("--operation-id, --occurrence-id, --kind, and --file are required"))
		}
		file, supplied, err := openMediaUpload(mediaFile)
		if err != nil {
			return err
		}
		defer func() { _ = file.Close() }()
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		receipt, err := c.ImportMediaArtifact(cmd.Context(), args[0], api.MediaArtifactMetadata{
			OperationID: mediaOperationID, OccurrenceID: mediaOccurrenceID, Kind: mediaArtifactKind,
			Origin: mediaOrigin, Provider: mediaProvider, Language: mediaLanguage,
			Filename: supplied.Filename, MediaType: supplied.MediaType, SHA256: supplied.SHA256,
			ByteLength: supplied.ByteLength}, file)
		if err != nil {
			return err
		}
		return writeCLIJSON(cmd.OutOrStdout(), receipt)
	}}

var mediaOccurrencesCmd = &cobra.Command{Use: "occurrences", Short: "Inspect or revoke recording occurrences",
	Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() }}

var mediaOccurrencesListCmd = &cobra.Command{Use: "list", Short: "List caller-visible occurrences",
	Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		page, err := c.MediaOccurrences(cmd.Context(), daemonconn.MediaOccurrenceOptions{
			Cursor: mediaCursor, Limit: mediaLimit, SourceID: mediaSourceID})
		if err != nil {
			return err
		}
		return writeCLIJSON(cmd.OutOrStdout(), page)
	}}

var mediaOccurrencesDeclareCmd = &cobra.Command{Use: "declare <source-id>", Short: "Declare an immutable occurrence revision",
	Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if err := requireMediaMutationFlags(); err != nil {
			return err
		}
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		receipt, err := c.DeclareMediaOccurrence(cmd.Context(), api.MediaOccurrenceMutationBody{
			OperationID: mediaOperationID, SourceID: args[0], Occurrence: mediaOccurrenceFlags("")})
		if err != nil {
			return err
		}
		return writeCLIJSON(cmd.OutOrStdout(), receipt)
	}}

var mediaOccurrencesRevokeCmd = &cobra.Command{Use: "revoke <occurrence-id>", Short: "Revoke one occurrence revision",
	Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if mediaOperationID == "" || mediaOccurrenceRevision == "" {
			return usageError(errors.New("--operation-id and --revision are required"))
		}
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		receipt, err := c.RevokeMediaOccurrence(cmd.Context(), args[0], api.MediaOccurrenceRevokeBody{
			OperationID: mediaOperationID, Revision: mediaOccurrenceRevision})
		if err != nil {
			return err
		}
		return writeCLIJSON(cmd.OutOrStdout(), receipt)
	}}

var mediaOriginsCmd = &cobra.Command{Use: "origins", Short: "List registered acquisition origins", Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		page, err := c.MediaOrigins(cmd.Context())
		if err != nil {
			return err
		}
		return writeCLIJSON(cmd.OutOrStdout(), page)
	}}

var mediaAcquisitionPlanCmd = &cobra.Command{Use: "acquisition-plan", Short: "Recognize a private reference without egress",
	Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if mediaReferenceFile == "" {
			return usageError(errors.New("--reference-file is required"))
		}
		reference, err := readPrivateReference(mediaReferenceFile)
		if err != nil {
			return err
		}
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		plan, err := c.PlanMediaAcquisition(cmd.Context(), api.MediaReferenceBody{ReferenceURL: reference})
		if err != nil {
			return err
		}
		return writeCLIJSON(cmd.OutOrStdout(), plan)
	}}

var mediaConsentCmd = &cobra.Command{Use: "consent", Short: "Grant or revoke acquisition consent",
	Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() }}

var mediaConsentGrantCmd = &cobra.Command{Use: "grant", Short: "Grant one exact acquisition plan",
	Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if mediaOperationID == "" || mediaPlanFile == "" {
			return usageError(errors.New("--operation-id and --plan-file are required"))
		}
		data, err := os.ReadFile(mediaPlanFile)
		if err != nil {
			return err
		}
		var plan api.MediaAcquisitionPlan
		if err := json.Unmarshal(data, &plan, json.RejectUnknownMembers(true)); err != nil || plan.PlanToken == "" {
			return usageError(errors.New("plan file is not a media acquisition plan"))
		}
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		receipt, err := c.GrantMediaAcquisition(cmd.Context(), api.MediaAcquisitionGrantBody{
			OperationID: mediaOperationID, PlanToken: plan.PlanToken})
		if err != nil {
			return err
		}
		return writeCLIJSON(cmd.OutOrStdout(), receipt)
	}}

var mediaConsentRevokeCmd = &cobra.Command{Use: "revoke", Short: "Revoke acquisition consent for one origin",
	Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if mediaOperationID == "" || mediaOrigin == "" {
			return usageError(errors.New("--operation-id and --origin are required"))
		}
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		receipt, err := c.RevokeMediaAcquisition(cmd.Context(), api.MediaAcquisitionRevokeBody{
			OperationID: mediaOperationID, OriginID: mediaOrigin})
		if err != nil {
			return err
		}
		return writeCLIJSON(cmd.OutOrStdout(), receipt)
	}}

func init() {
	mediaSubmitCmd.Flags().StringVar(&mediaFile, "file", "", "recording file path")
	mediaSubmitCmd.Flags().StringVar(&mediaReferenceFile, "reference-file", "", "private reference file path, or - for stdin")
	mediaSubmitCmd.Flags().BoolVar(&mediaAcquire, "acquire", false, "request separately consented acquisition")
	for _, command := range []*cobra.Command{mediaSubmitCmd, mediaOccurrencesDeclareCmd} {
		command.Flags().StringVar(&mediaOccurrenceRef, "occurrence-ref", "", "caller occurrence reference")
		command.Flags().StringVar(&mediaOccurrenceRevision, "revision", "", "caller occurrence revision")
		command.Flags().StringVar(&mediaPersonRef, "person-ref", "", "optional person reference")
		command.Flags().StringVar(&mediaSpeakerLabel, "speaker-label", "", "optional speaker label")
	}
	for _, command := range []*cobra.Command{mediaSubmitCmd, mediaRetryCmd} {
		command.Flags().StringVar(&mediaProfile, "processing-profile", "", "explicit processing profile")
		command.Flags().StringVar(&mediaInputID, "supplied-input-id", "", "exact supplied input identity")
	}
	for _, command := range []*cobra.Command{mediaSubmitCmd, mediaRetryCmd, mediaImportCmd,
		mediaOccurrencesDeclareCmd, mediaOccurrencesRevokeCmd, mediaConsentGrantCmd, mediaConsentRevokeCmd} {
		command.Flags().StringVar(&mediaOperationID, "operation-id", "", "idempotent UUIDv4 operation identity")
	}
	mediaListCmd.Flags().StringVar(&mediaCursor, "cursor", "", "opaque page cursor")
	mediaListCmd.Flags().IntVar(&mediaLimit, "limit", 100, "page size")
	mediaImportCmd.Flags().StringVar(&mediaFile, "file", "", "artifact file path")
	mediaImportCmd.Flags().StringVar(&mediaOccurrenceID, "occurrence-id", "", "bound occurrence identity")
	mediaImportCmd.Flags().StringVar(&mediaArtifactKind, "kind", "", "media, caption, or transcript")
	mediaImportCmd.Flags().StringVar(&mediaOrigin, "origin", "supplied", "artifact origin")
	mediaImportCmd.Flags().StringVar(&mediaProvider, "provider", "", "artifact provider")
	mediaImportCmd.Flags().StringVar(&mediaLanguage, "language", "", "source-supported language")
	mediaOccurrencesListCmd.Flags().StringVar(&mediaCursor, "cursor", "", "opaque page cursor")
	mediaOccurrencesListCmd.Flags().IntVar(&mediaLimit, "limit", 100, "page size")
	mediaOccurrencesListCmd.Flags().StringVar(&mediaSourceID, "source-id", "", "source filter")
	mediaOccurrencesRevokeCmd.Flags().StringVar(&mediaOccurrenceRevision, "revision", "", "expected caller revision")
	mediaAcquisitionPlanCmd.Flags().StringVar(&mediaReferenceFile, "reference-file", "", "private reference file path, or - for stdin")
	mediaConsentGrantCmd.Flags().StringVar(&mediaPlanFile, "plan-file", "", "JSON acquisition plan file")
	mediaConsentRevokeCmd.Flags().StringVar(&mediaOrigin, "origin", "", "registered origin ID")
	mediaOccurrencesCmd.AddCommand(mediaOccurrencesListCmd, mediaOccurrencesDeclareCmd, mediaOccurrencesRevokeCmd)
	mediaConsentCmd.AddCommand(mediaConsentGrantCmd, mediaConsentRevokeCmd)
	mediaCmd.AddCommand(mediaSubmitCmd, mediaListCmd, mediaStatusCmd, mediaRetryCmd, mediaImportCmd,
		mediaOccurrencesCmd, mediaOriginsCmd, mediaAcquisitionPlanCmd, mediaConsentCmd)
	rootCmd.AddCommand(mediaCmd)
}

func mediaSubmitFilename(file string) string {
	if file == "" {
		return ""
	}
	return filepath.Base(file)
}

func requireMediaMutationFlags() error {
	if mediaOperationID == "" || mediaOccurrenceRef == "" || mediaOccurrenceRevision == "" {
		return usageError(errors.New("--operation-id, --occurrence-ref, and --revision are required"))
	}
	return nil
}

func mediaOccurrenceFlags(filename string) api.MediaOccurrenceBody {
	return api.MediaOccurrenceBody{Ref: mediaOccurrenceRef, Revision: mediaOccurrenceRevision,
		Filename: filename, PersonRef: mediaPersonRef, SpeakerLabel: mediaSpeakerLabel}
}

func mediaProcessingFlags() *api.MediaProcessingBody {
	if mediaProfile == "" {
		return nil
	}
	return &api.MediaProcessingBody{Profile: mediaProfile, SuppliedInputID: mediaInputID}
}

var mediaUploadTypes = map[string]string{
	".mp3": "audio/mpeg", ".mp4": "video/mp4", ".srt": "application/x-subrip",
	".txt": "text/plain; charset=utf-8", ".wav": "audio/wav",
}

func openMediaUpload(path string) (*os.File, api.MediaSuppliedMetadata, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, api.MediaSuppliedMetadata{}, err
	}
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		_ = file.Close()
		return nil, api.MediaSuppliedMetadata{}, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		_ = file.Close()
		return nil, api.MediaSuppliedMetadata{}, err
	}
	mediaType := mediaUploadTypes[strings.ToLower(filepath.Ext(path))]
	if mediaType == "" {
		mediaType = mime.TypeByExtension(strings.ToLower(filepath.Ext(path)))
	}
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	return file, api.MediaSuppliedMetadata{Filename: filepath.Base(path), MediaType: mediaType,
		SHA256: hex.EncodeToString(hash.Sum(nil)), ByteLength: size}, nil
}

func readPrivateReference(path string) (string, error) {
	var reader io.Reader
	if path == "-" {
		reader = os.Stdin
	} else {
		file, err := os.Open(path)
		if err != nil {
			return "", err
		}
		defer func() { _ = file.Close() }()
		reader = file
	}
	data, err := io.ReadAll(io.LimitReader(reader, 8193))
	if err != nil || len(data) > 8192 {
		return "", errors.New("reading private media reference: bounded reference required")
	}
	value := strings.TrimSpace(string(data))
	if value == "" {
		return "", errors.New("private media reference is empty")
	}
	return value, nil
}
