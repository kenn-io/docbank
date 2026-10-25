variable "VERSION" { default = "dev" }
variable "OCI_VERSION" { default = "dev" }
variable "COMMIT" { default = "unknown" }
variable "REVISION" { default = "unknown" }
variable "BUILD_DATE" { default = "" }
variable "OCI_OUTPUT_DIR" { default = "./dist" }

target "oci" {
  name = "oci-${platform.architecture}"
  matrix = {
    platform = [
      {
        os           = "linux"
        architecture = "amd64"
      },
      {
        os           = "linux"
        architecture = "arm64"
      },
    ]
  }

  context    = "."
  dockerfile = "Dockerfile"
  platforms  = ["${platform.os}/${platform.architecture}"]

  args = {
    VERSION = VERSION
    COMMIT  = COMMIT
  }

  labels = {
    "org.opencontainers.image.created"     = BUILD_DATE
    "org.opencontainers.image.description" = "Local-first system of record for documents (PDFs, images, text files, etc.) for you and your agents"
    "org.opencontainers.image.licenses"    = "Apache-2.0"
    "org.opencontainers.image.revision"    = REVISION
    "org.opencontainers.image.source"      = "https://github.com/kenn-io/docbank"
    "org.opencontainers.image.title"       = "Docbank"
    "org.opencontainers.image.url"         = "https://github.com/kenn-io/docbank"
    "org.opencontainers.image.version"     = OCI_VERSION
  }

  output = [{
    type             = "oci"
    dest             = "${OCI_OUTPUT_DIR}/${platform.architecture}.oci.tar"
    "oci-mediatypes" = true
  }]
}
