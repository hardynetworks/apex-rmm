package tapo

// Vectors exposes the KLAP v2 primitives to the cross-check against python-kasa.
func Vectors(local, remote []byte, user, pass string, m1, m2 []byte) (auth, hs1, hs2 []byte, seq1 int32, p1 []byte, seq2 int32, p2 []byte) {
	auth = authHashV2(Credentials{Username: user, Password: pass})
	hs1, hs2 = hs1Hash(true, local, remote, auth), hs2Hash(true, local, remote, auth)
	s := newSession(local, remote, auth)
	p1, seq1 = s.encrypt(m1)
	p2, seq2 = s.encrypt(m2)
	return
}
