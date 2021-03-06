// Copyright 2019 Antrea Authors
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

package antctl

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog"
)

// requestOption describes options to issue requests to the antctl server.
type requestOption struct {
	groupVersion *schema.GroupVersion
	// path is the destination URL of the ongoing request.
	path *url.URL
	// kubeconfig is the path to the config file for kubectl.
	kubeconfig string
	// name is the command which is going to be requested.
	name string
	// timeout specifies a time limit for requests made by the client. The timeout
	// duration includes connection setup, all redirects, and reading of the
	// response body.
	timeOut time.Duration
}

// client issues requests to an antctl server and gets the response.
type client struct {
	// inPod indicate whether the client is running in a pod or not.
	inPod bool
	// codec is the CodecFactory for this command, it is needed for remote accessing.
	codec serializer.CodecFactory
}

// resolveKubeconfig tries to load the kubeconfig specified in the requestOption.
// It will return error if the stating of the file failed or the kubeconfig is malformed.
// It will not try to look up InCluster configuration. If the kubeconfig is loaded,
// the groupVersion and the codec in the requestOption will be populated into the
// kubeconfig object.
func (c *client) resolveKubeconfig(opt *requestOption) (*rest.Config, error) {
	kubeconfig, err := clientcmd.BuildConfigFromFlags("", opt.kubeconfig)
	if err != nil {
		return nil, err
	}
	kubeconfig.GroupVersion = opt.groupVersion
	kubeconfig.NegotiatedSerializer = serializer.DirectCodecFactory{CodecFactory: c.codec}
	return kubeconfig, nil
}

// localRequest issues the local request according to the requestOption. It only cares about
// the groupVersion of the requestOption which will be used to construct the request
// URI. localRequest is basically a raw http request, no authentication and authorization
// will be done during the request. For safety concerns, it communicates with the
// antctl server by a predefined unix domain socket. If the request succeeds, it
// will return an io.Reader which contains the response data.
func (c *client) localRequest(opt *requestOption) (io.Reader, error) {
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (conn net.Conn, err error) {
				if opt.timeOut != 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, opt.timeOut)
					defer cancel()
				}
				return new(net.Dialer).DialContext(ctx, "unix", unixDomainSockAddr)
			},
		},
	}
	opt.path.Host = "antctl"
	opt.path.Scheme = "http"
	resp, err := client.Get(opt.path.String())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	// Since we are going to close the connection, copying the response.
	var buf bytes.Buffer
	_, err = io.Copy(&buf, resp.Body)
	return &buf, err
}

// Request issues the appropriate request to the antctl server according to the
// request options. If the inPod field of the client is true, the client will do
// a local request by invoking localRequest. Otherwise, it will check the kubeconfig
// and delegate the request destined to the antctl server to the kubernetes API server.
// If the request succeeds, it will return an io.Reader which contains the response
// data.
func (c *client) Request(opt *requestOption) (io.Reader, error) {
	if c.inPod {
		klog.Infoln("antctl runs as local mode")
		return c.localRequest(opt)
	}
	kubeconfig, err := c.resolveKubeconfig(opt)
	if err != nil {
		return nil, err
	}
	restClient, err := rest.UnversionedRESTClientFor(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create rest client: %w", err)
	}
	// If timeout is not set, no timeout.
	restClient.Client.Timeout = opt.timeOut
	result := restClient.Get().RequestURI(opt.path.RequestURI()).Do()
	if result.Error() != nil {
		return nil, fmt.Errorf("error when requesting %s: %w", opt.path.RequestURI(), result.Error())
	}
	raw, err := result.Raw()
	if err != nil {
		return nil, err
	}
	return bytes.NewReader(raw), nil
}
