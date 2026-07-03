// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	retryablehttp "github.com/hashicorp/go-retryablehttp"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"
)

const (
	ErrOutputStringRequest = "output a string, please"
)

var LastOutputStringError *OutputStringError

type OutputStringError struct {
	*retryablehttp.Request
	TLSSkipVerify              bool
	ClientCACert, ClientCAPath string
	ClientCert, ClientKey      string
	finalCurlString            string
}

func (d *OutputStringError) Error() string {
	if d.finalCurlString == "" {
		cs, err := d.buildCurlString()
		if err != nil {
			return err.Error()
		}
		d.finalCurlString = cs
	}

	return ErrOutputStringRequest
}

func (d *OutputStringError) AsRequest() (string, error) {
	path := d.Request.URL.Path
	path, ok := strings.CutPrefix(path, "/v1/")
	if !ok {
		return "", fmt.Errorf("path %q does not start with /v1/", path)
	}

	body, err := d.BodyBytes()
	if err != nil {
		return "", err
	}

	data := ctyjson.SimpleJSONValue{}
	err = json.Unmarshal(body, &data)
	if err != nil {
		return "", err
	}

	var operation string
	switch d.Method {
	case "GET":
		operation = "read"

	case "POST":
		fallthrough
	case "PUT":
		operation = "update"

	case "DELETE":
		operation = "delete"
	}

	file := hclwrite.NewEmptyFile()
	request := file.Body().AppendNewBlock("request", []string{"generated"})
	request.Body().SetAttributeValue("path", cty.StringVal(path))
	request.Body().SetAttributeValue("operation", cty.StringVal(operation))
	request.Body().SetAttributeValue("data", data.Value)

	return string(file.Bytes()), nil
}

func (d *OutputStringError) CurlString() (string, error) {
	if d.finalCurlString == "" {
		cs, err := d.buildCurlString()
		if err != nil {
			return "", err
		}
		d.finalCurlString = cs
	}
	return d.finalCurlString, nil
}

func (d *OutputStringError) buildCurlString() (string, error) {
	body, err := d.BodyBytes()
	if err != nil {
		return "", err
	}

	// Build cURL string
	finalCurlString := "curl "
	if d.TLSSkipVerify {
		finalCurlString += "--insecure "
	}
	if d.Method != http.MethodGet {
		finalCurlString = fmt.Sprintf("%s-X %s ", finalCurlString, d.Method)
	}
	if d.ClientCACert != "" {
		clientCACert := strings.ReplaceAll(d.ClientCACert, "'", "'\"'\"'")
		finalCurlString = fmt.Sprintf("%s--cacert '%s' ", finalCurlString, clientCACert)
	}
	if d.ClientCAPath != "" {
		clientCAPath := strings.ReplaceAll(d.ClientCAPath, "'", "'\"'\"'")
		finalCurlString = fmt.Sprintf("%s--capath '%s' ", finalCurlString, clientCAPath)
	}
	if d.ClientCert != "" {
		clientCert := strings.ReplaceAll(d.ClientCert, "'", "'\"'\"'")
		finalCurlString = fmt.Sprintf("%s--cert '%s' ", finalCurlString, clientCert)
	}
	if d.ClientKey != "" {
		clientKey := strings.ReplaceAll(d.ClientKey, "'", "'\"'\"'")
		finalCurlString = fmt.Sprintf("%s--key '%s' ", finalCurlString, clientKey)
	}
	for k, v := range d.Header {
		for _, h := range v {
			if strings.ToLower(k) == "x-vault-token" {
				h = `$(bao print token)`
			}
			finalCurlString = fmt.Sprintf("%s-H \"%s: %s\" ", finalCurlString, k, h)
		}
	}

	if len(body) > 0 {
		// We need to escape single quotes since that's what we're using to
		// quote the body
		escapedBody := strings.ReplaceAll(string(body), "'", "'\"'\"'")
		finalCurlString = fmt.Sprintf("%s-d '%s' ", finalCurlString, escapedBody)
	}

	return fmt.Sprintf("%s%s", finalCurlString, strconv.Quote(d.URL.String())), nil
}
