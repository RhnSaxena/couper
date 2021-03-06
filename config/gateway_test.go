package config_test

import (
	"fmt"
	"testing"

	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hclwrite"

	"github.com/avenga/couper/config"
)

func TestWriteGateway(t *testing.T) {
	conf := config.Gateway{
		Server: []*config.Server{
			{
				Name: "wurst",
				API: &config.Api{
					BasePath: "/hans/v1",
					Endpoint: []*config.Endpoint{
						{
							Pattern: "/proxy/",
						},
					},
				},
			},
		},
	}
	f := hclwrite.NewEmptyFile()
	gohcl.EncodeIntoBody(&conf, f.Body())
	fmt.Printf("%s", f.Bytes())
}
