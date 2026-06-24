package sqs

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
)

func unmarshal(raw []byte, out any) error {
	return json.Unmarshal(raw, out)
}

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}
