// Copyright (c) 2019 Baidu, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package bfe_basic

import (
	"github.com/baidu/bfe/bfe_http"
	"github.com/baidu/bfe/bfe_route/bfe_cluster"
)

const (
	HeaderBfeIP         = "X-Bfe-Ip"
	HeaderBfeLogId      = "X-Bfe-Log-Id"
	HeaderForwardedFor  = "X-Forwarded-For"
	HeaderForwardedPort = "X-Forwarded-Port"
	HeaderRealIP        = "X-Real-Ip"
	HeaderRealPort      = "X-Real-Port"
)

type OperationStage int

const (
	StageStartConn OperationStage = iota
	StageReadReqHeader
	StageReadReqBody
	StageConnBackend
	StageWriteBackend
	StageReadResponseHeader
	StageReadResponseBody
	StageWriteClient
	StageEndRequest
)

// CreateInternalSrvErrResp returns a HTTP 500 response
func CreateInternalSrvErrResp(request *Request) *bfe_http.Response {
	return CreateInternalResp(request, bfe_http.StatusInternalServerError)
}

// CreateInternalSrvErrResp returns a HTTP 403 response
func CreateForbiddenResp(request *Request) *bfe_http.Response {
	return CreateInternalResp(request, bfe_http.StatusForbidden)
}

func CreateInternalResp(request *Request, code int) *bfe_http.Response {
        res := new(bfe_http.Response)
        res.StatusCode = code
        res.Header = make(bfe_http.Header)
        res.Header.Set("Server", "bfe")
        res.Body = bfe_http.EofReader
        request.HttpResponse = res
        return res
}

// this interface is used for lookup config for each request
type ServerDataConfInterface interface {
	ClusterTableLookup(clusterName string) (*bfe_cluster.BfeCluster, error)
	HostTableLookup(hostname string) (string, error)
}
