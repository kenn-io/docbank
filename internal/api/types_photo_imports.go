package api

type PhotoImportStartRequest struct {
	SourceRoot  string `json:"source_root" minLength:"1" maxLength:"4096"`
	Destination string `json:"destination" minLength:"1" maxLength:"4096"`
}
