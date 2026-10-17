package query

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPhotoFolderKey(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		kind, desc, path, key string
		valid                 bool
	}{
		{"cli", "first", "/shots/a.jpg", "97310aecd6d353ecc4b0b3bbe2a48333e4be26fa435ef8edbce032e1da9fd52d", true},
		{"cli", "second", "/shots/b.jpg", "97310aecd6d353ecc4b0b3bbe2a48333e4be26fa435ef8edbce032e1da9fd52d", true},
		{"cli", "", `C:\shots\a.jpg`, "18e6828f06efb295809a602b37c11176daeb34f7d3265b6cc6ce16452d69045c", true},
		{"cli", "", `C:/shots/b.jpg`, "18e6828f06efb295809a602b37c11176daeb34f7d3265b6cc6ce16452d69045c", true},
		{"cli", "", `\\server\share\shots\a.jpg`, "91e3194e643686be077ed2f491b312794e390dab130e3503cec02778b6d2590e", true},
		{"cli", "", `//server/share/shots/b.jpg`, "91e3194e643686be077ed2f491b312794e390dab130e3503cec02778b6d2590e", true},
		{"watch", "camera", "/shots/a.jpg", "24bdb20254864c349da512700a1fc77a465d1b042b60769fc3d187699ab9fad7", true},
		{"push", "camera", "/shots/a.jpg", "043d20f8124ffecbfe429e8d5bc753d4525262686a362ba160497eb228cb5e66", true},
		{"cli", "", `/a\b/c.jpg`, "8d1b8c42a2fbb61243390107301e8059f9b6ad65883c103274a9e4d3051403c3", true},
		{"cli", "", "/café/a.jpg", "a434c8fb44ab6b773c8921e9a11683a9139a2572c1cd1b2b65ab4eab3e2ffa5e", true},
		{"cli", "", "/cafe\u0301/a.jpg", "f3a0e078b8fa84d23a138562f9d1a970d22bd0ec49794c506f5cbf1a38f85a83", true},
		{"cli", "", "", "", false},
	} {
		key, valid := PhotoFolderKey(tc.kind, tc.desc, tc.path)
		require.Equal(t, tc.valid, valid, "%q", tc.path)
		require.Equal(t, tc.key, key, "%q", tc.path)
	}
}
