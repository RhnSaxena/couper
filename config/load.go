package config

import (
	"fmt"
	"io/ioutil"

	"github.com/hashicorp/hcl/v2/hclsimple"

	"go.avenga.cloud/couper/gateway/eval"
)

func LoadFile(filename string) (*Gateway, error) {
	src, err := ioutil.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("Failed to load configuration: %w", err)
	}
	return LoadBytes(src)
}

func LoadBytes(src []byte) (*Gateway, error) {
	config := &Gateway{Context: eval.NewENVContext(src)}
	// filename must match .hcl ending for further []byte processing
	if err := hclsimple.Decode("loadBytes.hcl", src, config.Context, config); err != nil {
		return nil, fmt.Errorf("Failed to load configuration bytes: %w", err)
	}
	return config, nil
}