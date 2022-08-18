package main

import (
	"encoding/json"
	"fmt"

	"github.com/cortexproject/cortex/tools/thanosconvert"
	"github.com/thanos-io/thanos/pkg/block/metadata"
)

func main() {

	str := `{
		"ulid": "01GARRGDJNMRDYCAYCW2HSP4ZG",
		"minTime": 1660807063253,
		"maxTime": 1660832062254,
		"stats": {
			"numSamples": 25000,
			"numSeries": 1,
			"numChunks": 212
		},
		"compaction": {
			"level": 1,
			"sources": [
				"01GARRGDJNMRDYCAYCW2HSP4ZG"
			]
		},
		"version": 1
	}`
	meta := metadata.Meta{}
	json.Unmarshal([]byte(str), &meta)

	ms, err := thanosconvert.ConvertMetadata(meta, "test")
	fmt.Println(ms, err)

	bs, _ := json.Marshal(meta)
	fmt.Println(string(bs))
}
