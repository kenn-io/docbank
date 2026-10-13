package main

import (
	"errors"
	"strconv"
	"strings"

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
	faces := &cobra.Command{Use: "faces", Short: "Record faces and people in photos", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() }}
	faces.AddCommand(&cobra.Command{Use: "list <content-version-id>", Short: "List face boxes", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
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
	var boxText, personID string
	create := &cobra.Command{Use: "create <content-version-id>", Short: "Mark a face box or a person in the whole photo", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		box, err := parsePhotoFaceBox(boxText)
		if err != nil {
			return err
		}
		if box == nil && personID == "" {
			return errors.New("provide --box or --person")
		}
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		result, err := withPhotoHidden(cmd, c, func(cookie string) (api.PhotoFaceResult, error) {
			return c.CreatePhotoFace(cmd.Context(), args[0], api.CreatePhotoFaceRequest{Box: box, PersonID: personID}, cookie)
		})
		if err != nil {
			return err
		}
		return writeCLIJSON(cmd.OutOrStdout(), result)
	}}
	create.Flags().StringVar(&boxText, "box", "", "x,y,width,height as fractions of the unrotated image")
	create.Flags().StringVar(&personID, "person", "", "person ID; omit --box to mark the whole photo")
	faces.AddCommand(create)
	return faces
}
