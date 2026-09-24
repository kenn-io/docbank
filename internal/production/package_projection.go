package production

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/loadfile"
)

const RecipientPackageContractV1 = "production-recipient-package/v1"

// Received package manifests admit at most 64 volumes.
const maxRecipientVolumes = 64

var ErrPackageProjection = errors.New("production package projection conflicts with published authority")

// PackageMember supplies the family boundary for one published occurrence.
// Its IDs are private planning inputs and never enter RecipientManifest.
type PackageMember struct {
	ID       string
	Ordinal  int64
	FamilyID string
}

type PackageLimits struct {
	MaxVolumeBytes     int64
	MaxVolumeDocuments int
}

// RecipientManifest is the complete typed public JSON projection. It has no
// generic metadata map or private source, member, artifact, or policy fields.
type RecipientManifest struct {
	Contract  string              `json:"contract"`
	ProfileID string              `json:"profile_id"`
	Volumes   []RecipientVolume   `json:"volumes"`
	Documents []RecipientDocument `json:"documents"`
}

type RecipientVolume struct {
	Name      string `json:"name"`
	Documents int    `json:"documents"`
	Pages     int    `json:"pages"`
	Bytes     int64  `json:"bytes"`
}

type RecipientDocument struct {
	Control    string           `json:"control"`
	End        string           `json:"end"`
	Volume     string           `json:"volume"`
	PDFPath    string           `json:"pdf_path,omitzero"`
	PDFSHA256  string           `json:"pdf_sha256,omitzero"`
	PDFSize    int64            `json:"pdf_size,omitzero"`
	TextPath   string           `json:"text_path"`
	TextSHA256 string           `json:"text_sha256"`
	TextSize   int64            `json:"text_size"`
	Pages      []RecipientPage  `json:"pages"`
	Images     []RecipientImage `json:"images,omitzero"`
}

type RecipientPage struct {
	Number string `json:"number"`
}

type RecipientImage struct {
	Number string `json:"number"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type packageBinding struct {
	path     string
	artifact documentproduction.Artifact
}

// PackageProjection keeps private artifact bindings separate from the public
// manifest. The archive writer consumes bindings; only Manifest is serialized.
type PackageProjection struct {
	Manifest       RecipientManifest
	jobID          string
	export         loadfile.ProductionExportPlan
	bindings       []packageBinding
	manifestSHA256 string
	bindingsSHA256 string
}

func (p PackageProjection) PageNumbers() []string {
	result := make([]string, 0)
	for _, doc := range p.Manifest.Documents {
		for _, page := range doc.Pages {
			result = append(result, page.Number)
		}
	}
	return result
}

type packageArtifactSet struct {
	pdf   *documentproduction.Artifact
	text  *documentproduction.Artifact
	pages map[int]documentproduction.Artifact
}

// PlanPackageProjection validates a successful published job and builds the
// recipient allowlist in occurrence order. It never resolves a missing output
// from an original, native, source PDF, or source text representation.
func PlanPackageProjection(job Job, reservation documentproduction.NumberReservation,
	members []PackageMember, profileID string, limits PackageLimits) (PackageProjection, error) {
	bad := func() (PackageProjection, error) { return PackageProjection{}, ErrPackageProjection }
	if job.State != ProductionJobSucceeded || job.ID == "" || job.Receipt.ID != job.ID || job.Receipt.JobID != job.ID ||
		job.Receipt.SetID != job.SetID || job.Receipt.Revision != job.Revision ||
		job.Receipt.RevisionSHA256 != job.RevisionSHA256 ||
		job.Receipt.PreparedInputSHA256 != job.PreparedInputSHA256 ||
		job.Receipt.ArtifactManifestSHA256 != job.Manifest.SHA256 ||
		job.Receipt.NumberReservationSHA256 != reservation.SHA256 ||
		reservation.OperationID != job.ID || reservation.RevisionSHA256 != job.RevisionSHA256 ||
		documentproduction.ValidateProductionReceipt(job.Receipt) != nil ||
		documentproduction.ValidateArtifactManifest(job.Manifest) != nil ||
		documentproduction.ValidateNumberReservation(reservation) != nil ||
		len(members) == 0 || len(members) > 100_000 ||
		limits.MaxVolumeBytes < 1 || limits.MaxVolumeBytes > 50<<30 ||
		limits.MaxVolumeDocuments < 1 || limits.MaxVolumeDocuments > 100_000 {
		return bad()
	}
	roles := []string{documentproduction.ArtifactRoleRedactedPDF, documentproduction.ArtifactRoleRedactedText}
	imageProfile := profileID == "export-dat-opt-images-v1" || profileID == "export-dat-lfp-images-v1"
	if imageProfile {
		roles = []string{documentproduction.ArtifactRoleRedactedPage, documentproduction.ArtifactRoleRedactedText}
	}
	exportPlan, err := loadfile.PlanProductionExport(profileID, roles, false)
	if err != nil {
		return PackageProjection{}, err
	}
	ordered := slices.Clone(members)
	slices.SortFunc(ordered, func(a, b PackageMember) int {
		switch {
		case a.Ordinal < b.Ordinal:
			return -1
		case a.Ordinal > b.Ordinal:
			return 1
		default:
			return 0
		}
	})
	byID := make(map[string]PackageMember, len(ordered))
	for index, member := range ordered {
		if member.ID == "" || member.Ordinal != int64(index+1) || member.FamilyID == "" {
			return bad()
		}
		if _, duplicate := byID[member.ID]; duplicate {
			return bad()
		}
		byID[member.ID] = member
	}
	sets := make(map[string]*packageArtifactSet, len(ordered))
	for _, artifact := range job.Manifest.Artifacts {
		member, ok := byID[artifact.MemberID]
		if !ok || artifact.MemberOrdinal != member.Ordinal || artifact.Size < 1 {
			return bad()
		}
		set := sets[member.ID]
		if set == nil {
			set = &packageArtifactSet{pages: map[int]documentproduction.Artifact{}}
			sets[member.ID] = set
		}
		switch artifact.Role {
		case documentproduction.ArtifactRoleRedactedPDF:
			if set.pdf != nil || artifact.Page != 0 || artifact.MediaType != "application/pdf" {
				return bad()
			}
			selected := artifact
			set.pdf = &selected
		case documentproduction.ArtifactRoleRedactedText:
			if set.text != nil || artifact.Page != 0 || artifact.MediaType != "text/plain; charset=utf-8" {
				return bad()
			}
			selected := artifact
			set.text = &selected
		case documentproduction.ArtifactRoleRedactedPage:
			if artifact.Page < 1 || artifact.MediaType != "image/png" || set.pages[artifact.Page].ID != "" {
				return bad()
			}
			set.pages[artifact.Page] = artifact
		default:
			return bad()
		}
	}
	pageLabels := make(map[string][]string, len(ordered))
	orderedNumbers := slices.Clone(reservation.Numbers)
	slices.SortFunc(orderedNumbers, func(a, b documentproduction.AssignedNumber) int {
		switch {
		case a.MemberOrdinal < b.MemberOrdinal:
			return -1
		case a.MemberOrdinal > b.MemberOrdinal:
			return 1
		case a.Page < b.Page:
			return -1
		case a.Page > b.Page:
			return 1
		default:
			return 0
		}
	})
	for _, number := range orderedNumbers {
		member, ok := byID[number.MemberID]
		if !ok || member.Ordinal != number.MemberOrdinal || !safePackagePageLabel(number.Text, profileID) ||
			number.Page != len(pageLabels[number.MemberID])+1 {
			return bad()
		}
		pageLabels[number.MemberID] = append(pageLabels[number.MemberID], number.Text)
	}
	result := PackageProjection{Manifest: RecipientManifest{
		Contract: RecipientPackageContractV1, ProfileID: profileID,
		Volumes: []RecipientVolume{}, Documents: []RecipientDocument{},
	}, jobID: job.ID, export: exportPlan}
	seenFamilies := map[string]bool{}
	for start := 0; start < len(ordered); {
		familyID := ordered[start].FamilyID
		if seenFamilies[familyID] {
			return bad()
		}
		seenFamilies[familyID] = true
		end := start + 1
		for end < len(ordered) && ordered[end].FamilyID == familyID {
			end++
		}
		familyBytes, familyPages := int64(0), 0
		for _, member := range ordered[start:end] {
			set := sets[member.ID]
			labels := pageLabels[member.ID]
			if set == nil || set.pdf == nil || set.text == nil || len(labels) < 1 || len(set.pages) != len(labels) {
				return bad()
			}
			for page := range labels {
				if set.pages[page+1].ID == "" {
					return bad()
				}
			}
			selectedBytes := set.text.Size
			if !imageProfile {
				if set.pdf.Size > limits.MaxVolumeBytes-familyBytes-selectedBytes {
					return bad()
				}
				selectedBytes += set.pdf.Size
			}
			// The PDF profile also declares OPT. Its page map must point to
			// actual redacted page images, never to a PDF repeated per page.
			for page := range labels {
				if set.pages[page+1].Size > limits.MaxVolumeBytes-familyBytes-selectedBytes {
					return bad()
				}
				selectedBytes += set.pages[page+1].Size
			}
			if selectedBytes < 1 || selectedBytes > limits.MaxVolumeBytes-familyBytes {
				return bad()
			}
			familyBytes += selectedBytes
			familyPages += len(labels)
		}
		if end-start > limits.MaxVolumeDocuments {
			return bad()
		}
		volumes := &result.Manifest.Volumes
		if len(*volumes) == 0 || (*volumes)[len(*volumes)-1].Bytes > limits.MaxVolumeBytes-familyBytes ||
			(*volumes)[len(*volumes)-1].Documents > limits.MaxVolumeDocuments-(end-start) {
			if len(*volumes) == maxRecipientVolumes {
				return bad()
			}
			*volumes = append(*volumes, RecipientVolume{Name: fmt.Sprintf("VOL%03d", len(*volumes)+1)})
		}
		volume := &(*volumes)[len(*volumes)-1]
		volume.Bytes += familyBytes
		volume.Documents += end - start
		volume.Pages += familyPages
		for _, member := range ordered[start:end] {
			set, labels := sets[member.ID], pageLabels[member.ID]
			stem := fmt.Sprintf("DOC%06d", member.Ordinal)
			doc := RecipientDocument{Control: labels[0], End: labels[len(labels)-1], Volume: volume.Name,
				TextPath: "TEXT/" + stem + ".txt", TextSHA256: set.text.SHA256,
				TextSize: set.text.Size, Pages: make([]RecipientPage, len(labels))}
			result.bindings = append(result.bindings, packageBinding{path: volume.Name + "/" + doc.TextPath, artifact: *set.text})
			if !imageProfile {
				doc.PDFPath = "PDF/" + stem + ".pdf"
				doc.PDFSHA256, doc.PDFSize = set.pdf.SHA256, set.pdf.Size
				result.bindings = append(result.bindings, packageBinding{path: volume.Name + "/" + doc.PDFPath, artifact: *set.pdf})
			}
			doc.Images = make([]RecipientImage, len(labels))
			for page, number := range labels {
				imagePath := fmt.Sprintf("IMAGES/%s-%06d.png", stem, page+1)
				artifact := set.pages[page+1]
				doc.Images[page] = RecipientImage{Number: number, Path: imagePath,
					SHA256: artifact.SHA256, Size: artifact.Size}
				result.bindings = append(result.bindings, packageBinding{path: volume.Name + "/" + imagePath, artifact: set.pages[page+1]})
			}
			for page, number := range labels {
				doc.Pages[page] = RecipientPage{Number: number}
			}
			result.Manifest.Documents = append(result.Manifest.Documents, doc)
		}
		start = end
	}
	if !recipientPackageFitsImportBudget(result.Manifest) {
		return bad()
	}
	encoded, err := canonical.Marshal(result.Manifest)
	if err != nil {
		return bad()
	}
	digest := sha256.Sum256(encoded)
	result.manifestSHA256 = hex.EncodeToString(digest[:])
	result.bindingsSHA256, err = packageBindingsDigest(result.bindings)
	if err != nil {
		return bad()
	}
	return result, nil
}

func packageBindingsDigest(bindings []packageBinding) (string, error) {
	sealed := make([]struct {
		Path     string                      `json:"path"`
		Artifact documentproduction.Artifact `json:"artifact"`
	}, len(bindings))
	for index, binding := range bindings {
		sealed[index].Path, sealed[index].Artifact = binding.path, binding.artifact
	}
	data, err := canonical.Marshal(sealed)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func safeRecipientLabel(value string) bool {
	if value == "" || len(value) > 256 || !utf8.ValidString(value) {
		return false
	}
	for _, char := range value {
		if unicode.IsControl(char) || unicode.Is(unicode.Cf, char) {
			return false
		}
	}
	return true
}

func safePackagePageLabel(value, profileID string) bool {
	if !safeRecipientLabel(value) || strings.ContainsAny(value, ",®") {
		return false
	}
	return profileID != "export-dat-lfp-images-v1" || !strings.ContainsAny(value, ";@")
}
