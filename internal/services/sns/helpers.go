package sns

import "encoding/json"

func unmarshal(raw []byte, out any) error {
	return json.Unmarshal(raw, out)
}
