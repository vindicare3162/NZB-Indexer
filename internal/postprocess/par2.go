package postprocess

import (
	"bytes"
	"encoding/binary"
	"errors"
)

// PAR2 packet framing constants. See the PAR2 spec: every packet begins with
// the 8-byte magic, then a 64-bit little-endian packet length, a 16-byte MD5
// packet hash, a 16-byte recovery-set ID, and a 16-byte packet type.
var (
	par2Magic          = []byte("PAR2\x00PKT")
	par2FileDescType   = []byte("PAR 2.0\x00FileDesc")
	par2MainType       = []byte("PAR 2.0\x00Main\x00\x00\x00\x00")
	errPar2NoFilenames = errors.New("postprocess: no filenames found in PAR2 data")
)

const par2HeaderLen = 8 + 8 + 16 + 16 + 16 // magic+len+hash+setid+type = 64

// HasPar2Magic reports whether data appears to be (or contain) PAR2 packets by
// looking for the PAR2 packet magic. Used to identify PAR2 bodies when the
// article subject gives no filename hint (obfuscated posts).
func HasPar2Magic(data []byte) bool {
	return bytes.Contains(data, par2Magic)
}

// ParsePar2Filenames extracts the set of real filenames declared in PAR2
// File Description packets. The data may contain many concatenated packets;
// duplicate filenames (repeated across recovery volumes) are de-duplicated
// while preserving first-seen order.
//
// A File Description packet body layout (after the 64-byte header) is:
//
//	16 bytes  File ID (MD5)
//	16 bytes  MD5 of the entire file
//	16 bytes  MD5-16k (first 16KiB)
//	 8 bytes  file length
//	 n bytes  ASCII filename (padded with NULs to a multiple of 4)
func ParsePar2Filenames(data []byte) ([]string, error) {
	var names []string
	seen := map[string]bool{}

	for offset := 0; offset+par2HeaderLen <= len(data); {
		idx := bytes.Index(data[offset:], par2Magic)
		if idx < 0 {
			break
		}
		start := offset + idx

		if start+par2HeaderLen > len(data) {
			break
		}
		pktLen := binary.LittleEndian.Uint64(data[start+8 : start+16])
		if pktLen < par2HeaderLen || start+int(pktLen) > len(data) {
			// Corrupt or truncated length; skip past this magic and continue.
			offset = start + len(par2Magic)
			continue
		}
		// Packet type occupies the 16 bytes at offset 48 (after magic[8] +
		// length[8] + packet-hash[16] + recovery-set-id[16]).
		pktType := data[start+48 : start+64]
		body := data[start+par2HeaderLen : start+int(pktLen)]

		if bytes.Equal(pktType, par2FileDescType) {
			if name := parseFileDescName(body); name != "" && !seen[name] {
				seen[name] = true
				names = append(names, name)
			}
		}

		offset = start + int(pktLen)
	}

	if len(names) == 0 {
		return nil, errPar2NoFilenames
	}
	return names, nil
}

// parseFileDescName extracts the filename from a File Description packet body.
func parseFileDescName(body []byte) string {
	// 16 (fileID) + 16 (md5) + 16 (md5-16k) + 8 (length) = 56 bytes precede
	// the filename.
	const nameOffset = 16 + 16 + 16 + 8
	if len(body) <= nameOffset {
		return ""
	}
	raw := body[nameOffset:]
	// Trim trailing NUL padding.
	raw = bytes.TrimRight(raw, "\x00")
	return string(raw)
}

// IsMainPacketType reports whether a 16-byte type identifies a PAR2 Main
// packet (exposed for potential future use / tests).
func IsMainPacketType(t []byte) bool {
	return bytes.Equal(t, par2MainType)
}


