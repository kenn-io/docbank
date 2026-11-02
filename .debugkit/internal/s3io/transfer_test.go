package s3io

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListDecodesObjectNamesOnce(t *testing.T) {
	for _, tt := range []struct {
		name, encoded, want string
		invalid             bool
		unencoded           bool
	}{
		{name: "control and literal escapes", encoded: "namespace%2F%01%2Bliteral%252F%25%2Fname", want: "namespace/\x01+literal%2F%/name"},
		{name: "plus encoded space", encoded: "name+space%2Fa+b%2Bc", want: "name space/a b+c"},
		{name: "percent encoded space", encoded: "name%20space%2Fa%20b%2Bc", want: "name space/a b+c"},
		{name: "unencoded literal escapes", encoded: "namespace/a+b%2F%zz", want: "namespace/a+b%2F%zz", unencoded: true},
		{name: "invalid escape", encoded: "namespace%2Fbad%zz", invalid: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "url", r.URL.Query().Get("encoding-type"))
				assert.Equal(t, "previous/%2B+", r.URL.Query().Get("continuation-token"))
				encoding := "<EncodingType>url</EncodingType>"
				if tt.unencoded {
					encoding = ""
				}
				_, err := w.Write([]byte("<ListBucketResult>" + encoding + "<IsTruncated>true</IsTruncated><NextContinuationToken>next/%2F+</NextContinuationToken><Contents><Key>" + tt.encoded + "</Key><Size>6</Size></Contents></ListBucketResult>"))
				assert.NoError(t, err)
			}))
			defer server.Close()
			client := s3.NewFromConfig(aws.Config{Region: "us-east-1", Credentials: credentials.NewStaticCredentialsProvider("test", "test-secret", "")}, func(o *s3.Options) {
				o.BaseEndpoint = aws.String(server.URL)
				o.UsePathStyle = true
			})
			page, err := List(t.Context(), client, "bucket", "namespace/", "previous/%2B+", 1)
			if tt.invalid {
				require.Error(t, err)
				assert.Nil(t, page)
				return
			}
			require.NoError(t, err)
			require.Len(t, page.Contents, 1)
			assert.Equal(t, tt.want, aws.ToString(page.Contents[0].Key))
			assert.Equal(t, "next/%2F+", aws.ToString(page.NextContinuationToken))
		})
	}
}
