package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

func parsePhotoFaceBox(value string) (*document.FaceBox, error) {
	if value == "" {
		return nil, nil //nolint:nilnil // An omitted box marks the whole photo.
	}
	parts := strings.Split(value, ",")
	if len(parts) != 4 {
		return nil, errors.New("box must be x,y,width,height in normalized coordinates")
	}
	var values [4]float64
	for i, part := range parts {
		parsed, err := strconv.ParseFloat(strings.TrimSpace(part), 64)
		if err != nil {
			return nil, errors.New("box must contain four numbers")
		}
		values[i] = parsed
	}
	box := document.FaceBox{X: values[0], Y: values[1], Width: values[2], Height: values[3]}
	if err := box.Validate(); err != nil {
		return nil, err
	}
	return &box, nil
}

func photoFacesCommand() *cobra.Command {
	faces := &cobra.Command{Long: "Create a face box or mark a person in the whole photo; list reads both.\nUse current_version_id from docbank stat <path-or-id> --json.", Use: "faces", Short: "Record faces and people in photos", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() }}
	faces.AddCommand(&cobra.Command{Use: "list <content-version-id>", Example: "  docbank photos faces list 11111111-1111-4111-8111-111111111111", Short: "List face boxes and people", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		page, err := withPhotoHidden(cmd, c, func(cookie string) (api.PhotoFacePage, error) { return c.PhotoFaces(cmd.Context(), args[0], cookie) })
		if err != nil {
			return err
		}
		return writeCLIJSON(cmd.OutOrStdout(), page)
	}})
	var boxText, personID, faceID string
	create := &cobra.Command{Use: "create <content-version-id>", Example: "  docbank photos faces create 11111111-1111-4111-8111-111111111111 --box 0.1,0.2,0.3,0.4", Short: "Mark a face box or a person in the whole photo", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		box, err := parsePhotoFaceBox(boxText)
		if err != nil {
			return err
		}
		if box == nil && personID == "" {
			return errors.New("provide --box or --person")
		}
		request, err := photoFaceCreateRequest(box, personID, faceID)
		if err != nil {
			return err
		}
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		result, err := withPhotoHidden(cmd, c, func(cookie string) (api.PhotoFaceResult, error) {
			return c.CreatePhotoFace(cmd.Context(), args[0], request, cookie)
		})
		if err != nil {
			return photoFaceCreateError(request.FaceID, err)
		}
		return writeCLIJSON(cmd.OutOrStdout(), result)
	}}
	create.Flags().StringVar(&boxText, "box", "", "x,y,width,height as fractions of the EXIF-oriented preview before rotation")
	create.Flags().StringVar(&personID, "person", "", "person ID to name a face; omit --box to mark the whole photo")
	create.Flags().StringVar(&faceID, "face-id", "", "face UUID for retrying or naming an existing box")
	faces.AddCommand(create)
	addPhotoFaceDecisionCommands(faces)
	return faces
}

func photoFaceCreateRequest(box *document.FaceBox, personID, faceID string) (api.CreatePhotoFaceRequest, error) {
	if box == nil && faceID != "" {
		return api.CreatePhotoFaceRequest{}, errors.New("--face-id requires --box")
	}
	if box != nil && faceID == "" {
		faceID = uuid.New().String()
	}
	if faceID != "" && !daemonconn.IsCanonicalUUIDv4(faceID) {
		return api.CreatePhotoFaceRequest{}, errors.New("face ID must be a canonical UUIDv4")
	}
	return api.CreatePhotoFaceRequest{FaceID: faceID, Box: box, PersonID: personID}, nil
}

func photoFaceCreateError(faceID string, err error) error {
	if faceID != "" && daemonconn.IsRetryableMutationError(err) {
		return fmt.Errorf("face create failed; retry with --face-id %s: %w", faceID, err)
	}
	return err
}

func addPhotoFaceDecisionCommands(faces *cobra.Command) {
	for _, action := range []string{"name", "retract", "reject", "dismiss", "key", "mark", "retract_mark"} {
		var request api.PhotoFaceDecisionRequest
		request.Action = action
		command := &cobra.Command{Use: strings.ReplaceAll(action, "_", "-") + " <content-version-id>", Short: map[string]string{"name": "Name a face", "retract": "Retract a face name", "reject": "Reject a face and person match", "dismiss": "Dismiss a face", "key": "Set or clear a person's key face", "mark": "Mark a person in the whole photo", "retract_mark": "Retract a whole-photo mark"}[action], Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			c, err := daemonconn.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			result, err := withPhotoHidden(cmd, c, func(cookie string) (api.PhotoFaceDecisionResult, error) {
				return c.DecidePhotoFace(cmd.Context(), args[0], request, cookie)
			})
			if err != nil {
				return err
			}
			return writeCLIJSON(cmd.OutOrStdout(), result)
		}}
		command.Flags().Int64Var(&request.Revision, "revision", 0, "expected face revision, or assertion revision for a whole-photo mark")
		_ = command.MarkFlagRequired("revision")
		if action != "mark" && action != "retract_mark" {
			command.Flags().StringVar(&request.FaceID, "face", "", "face UUID")
			if action != "key" {
				_ = command.MarkFlagRequired("face")
			}
		}
		if action == "name" || action == "reject" || action == "key" || action == "mark" || action == "retract_mark" {
			command.Flags().StringVar(&request.PersonID, "person", "", "person UUID")
			_ = command.MarkFlagRequired("person")
		}
		if action == "key" {
			command.Flags().Int64Var(&request.PersonRevision, "person-revision", 0, "expected person revision; omit --face to clear the key face")
			_ = command.MarkFlagRequired("person-revision")
		}
		faces.AddCommand(command)
	}
	faces.AddCommand(&cobra.Command{Use: "undo <receipt-id>", Short: "Undo a face decision", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		result, err := withPhotoHidden(cmd, c, func(cookie string) (api.PhotoFaceDecisionResult, error) {
			return c.UndoPhotoFaceDecision(cmd.Context(), args[0], cookie)
		})
		if err != nil {
			return err
		}
		return writeCLIJSON(cmd.OutOrStdout(), result)
	}})
}
