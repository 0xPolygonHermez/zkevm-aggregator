package rpclient

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

const jsonRPCVersion = "2.0"

// Request is a jsonrpc request
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response is a jsonrpc  success response
type Response struct {
	JSONRPC string
	ID      interface{}
	Result  json.RawMessage
}

// RPCClient is a client for the JSON RPC
type RPCClient struct {
	url string
}

// NewRPCClient creates an instance of client
func NewRPCClient(url string) *RPCClient {
	return &RPCClient{
		url: url,
	}
}

// JSONRPCCall executes a 2.0 JSON RPC HTTP Post Request to the provided URL with
// the provided method and parameters, which is compatible with the Ethereum
// JSON RPC Server.
func JSONRPCCall(url, method string, httpHeaders map[string]string, id interface{}, parameters ...interface{}) (Response, error) {
	params, err := json.Marshal(parameters)
	if err != nil {
		return Response{}, err
	}

	request := Request{
		JSONRPC: jsonRPCVersion,
		ID:      id,
		Method:  method,
		Params:  params,
	}

	httpRes, err := sendJSONRPC_HTTPRequest(url, request, httpHeaders)
	if err != nil {
		return Response{}, err
	}

	resBody, err := io.ReadAll(httpRes.Body)
	if err != nil {
		return Response{}, err
	}
	defer httpRes.Body.Close()

	if httpRes.StatusCode != http.StatusOK {
		return Response{}, fmt.Errorf("%v - %v", httpRes.StatusCode, string(resBody))
	}

	var res Response
	err = json.Unmarshal(resBody, &res)
	if err != nil {
		return Response{}, err
	}
	return res, nil
}

func sendJSONRPC_HTTPRequest(url string, payload interface{}, httpHeaders map[string]string) (*http.Response, error) {
	reqBody, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	reqBodyReader := bytes.NewReader(reqBody)
	httpReq, err := http.NewRequest(http.MethodPost, url, reqBodyReader)
	if err != nil {
		return nil, err
	}

	httpReq.Header.Add("Content-type", "application/json")
	for key, value := range httpHeaders {
		httpReq.Header.Add(key, value)
	}

	httpRes, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, err
	}

	return httpRes, nil
}
