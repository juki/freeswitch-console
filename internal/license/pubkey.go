package license

import "crypto/ed25519"

// publicKeyBase64 is the DEVELOPMENT signing key; its private half is
// docs/license-dev/private.key, outside the repository. Replace it with the
// release key before the first release, or anyone with the development key
// can issue licences.
const publicKeyBase64 = "2Yv4x4ziLQUb+ybPt/HAEXNgmMOa4aLdbY0GHACA/LU="

var publicKey = mustPublicKey(publicKeyBase64)

func mustPublicKey(s string) ed25519.PublicKey {
	k, err := ParsePublicKey(s)
	if err != nil {
		panic(err)
	}
	return k
}
