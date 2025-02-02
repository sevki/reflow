// Copyright 2017 GRAIL, Inc. All rights reserved.
// Use of this source code is governed by the Apache 2.0
// license that can be found in the LICENSE file.

package reflowlet

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	dockerclient "github.com/docker/docker/client"
	"github.com/grailbio/base/digest"
	"github.com/grailbio/base/errors"
	"github.com/grailbio/infra"
	infraaws "github.com/grailbio/infra/aws"
	infratls "github.com/grailbio/infra/tls"
	"github.com/grailbio/reflow"
	"github.com/grailbio/reflow/blob"
	"github.com/grailbio/reflow/blob/s3blob"
	"github.com/grailbio/reflow/ec2authenticator"
	"github.com/grailbio/reflow/internal/execimage"
	"github.com/grailbio/reflow/local"
	"github.com/grailbio/reflow/log"
	"github.com/grailbio/reflow/pool/server"
	"github.com/grailbio/reflow/repository/blobrepo"
	repositoryhttp "github.com/grailbio/reflow/repository/http"
	"github.com/grailbio/reflow/rest"
	"golang.org/x/net/http2"
	yaml "gopkg.in/yaml.v2"
)

// maxConcurrentStreams is the number of concurrent http/2 streams we
// support.
const maxConcurrentStreams = 20000

// A Server is a reflow server, exposing a local pool over an HTTP server.
type Server struct {
	// Schema is the infra schema.
	Schema infra.Schema
	// SchemaKeys contains the schema providers and flags.
	SchemaKeys infra.Keys
	// The server's config.
	// TODO(marius): move most of what is now flags here into the config.
	Config infra.Config

	// Addr is the address on which to listen.
	Addr string
	// Prefix is the prefix used for directory lookup; permits reflowlet
	// to run inside of Docker.
	Prefix string
	// Insecure listens on HTTP, not HTTPS.
	Insecure bool
	// Dir is the runtime data directory.
	Dir string
	// EC2Cluster tells whether this reflowlet is part of an EC2cluster.
	// When true, the reflowlet shuts down if it is idle after 10 minutes.
	EC2Cluster bool
	// HTTPDebug determines whether HTTP debug logging is turned on.
	HTTPDebug bool

	configFlag string

	// version of the reflowlet instance.
	version string
}

// NewServer returns a new server with specified version.
func NewServer(version string) *Server {
	return &Server{version: version}
}

// AddFlags adds flags configuring various Reflowlet parameters to
// the provided FlagSet.
func (s *Server) AddFlags(flags *flag.FlagSet) {
	flags.StringVar(&s.configFlag, "config", "", "the Reflow configuration file")
	flags.StringVar(&s.Addr, "addr", ":9000", "HTTPS server address")
	flags.StringVar(&s.Prefix, "prefix", "", "prefix used for directory lookup")
	flags.BoolVar(&s.Insecure, "insecure", false, "listen on HTTP, not HTTPS")
	flags.StringVar(&s.Dir, "dir", "/mnt/data/reflow", "runtime data directory")
	flags.BoolVar(&s.EC2Cluster, "ec2cluster", false, "this reflowlet is part of an ec2cluster")
	flags.BoolVar(&s.HTTPDebug, "httpdebug", false, "turn on HTTP debug logging")
}

// setTags sets the reflowlet version/digest tags on the EC2 instance (if running on one).
// This is based on AWS instance metadata retrievable as per:
// https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/ec2-instance-metadata.html#instancedata-data-retrieval
func (s *Server) setTags() error {
	if !s.EC2Cluster {
		return nil
	}
	resp, err := http.Get("http://169.254.169.254/latest/meta-data/instance-id")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	iid := string(b)
	digest, err := execimage.ImageDigest()
	if err != nil {
		return err
	}
	var sess *session.Session
	err = s.Config.Instance(&sess)
	if err != nil {
		return err
	}
	svc := ec2.New(sess, &aws.Config{MaxRetries: aws.Int(3)})
	input := &ec2.CreateTagsInput{
		Resources: []*string{aws.String(iid)},
		Tags: []*ec2.Tag{
			{Key: aws.String("reflowlet:version"), Value: aws.String(s.version)},
			{Key: aws.String("reflowlet:digest"), Value: aws.String(digest.String())}},
	}
	_, err = svc.CreateTags(input)
	return err
}

// ListenAndServe serves the Reflowlet server on the configured address.
func (s *Server) ListenAndServe() error {
	if s.configFlag != "" {
		b, err := ioutil.ReadFile(s.configFlag)
		if err != nil {
			return err
		}
		keys := make(infra.Keys)
		if err := yaml.Unmarshal(b, keys); err != nil {
			return fmt.Errorf("config %v: %v", s.configFlag, err)
		}
		for k, v := range keys {
			s.SchemaKeys[k] = v
		}
	}

	var err error
	s.Config, err = s.Schema.Make(s.SchemaKeys)
	if err != nil {
		return err
	}
	addr := os.Getenv("DOCKER_HOST")
	if addr == "" {
		addr = "unix:///var/run/docker.sock"
	}
	client, err := dockerclient.NewClient(
		addr, "1.22",
		nil, map[string]string{"user-agent": "reflow"})
	if err != nil {
		return err
	}

	var sess *session.Session
	err = s.Config.Instance(&sess)
	if err != nil {
		return err
	}
	var tlsa *infratls.Authority
	err = s.Config.Instance(&tlsa)
	if err != nil {
		return err
	}
	clientConfig, serverConfig, err := tlsa.HTTPS()
	if err != nil {
		return err
	}
	var creds *credentials.Credentials
	err = s.Config.Instance(&creds)
	if err != nil {
		return err
	}
	var tool *infraaws.AWSTool
	err = s.Config.Instance(&tool)
	if err != nil {
		return err
	}

	if err := s.setTags(); err != nil {
		return fmt.Errorf("set tags: %v", err)
	}

	// Default HTTPS and s3 clients for repository dialers.
	// TODO(marius): handle this more elegantly, perhaps by
	// avoiding global registration altogether.
	blobrepo.Register("s3", s3blob.New(sess))
	transport := &http.Transport{TLSClientConfig: clientConfig}
	http2.ConfigureTransport(transport)
	repositoryhttp.HTTPClient = &http.Client{Transport: transport}

	p := &local.Pool{
		Client:        client,
		Dir:           s.Dir,
		Prefix:        s.Prefix,
		Authenticator: ec2authenticator.New(sess),
		AWSImage:      string(*tool),
		AWSCreds:      creds,
		Blob: blob.Mux{
			"s3": s3blob.New(sess),
		},
		Log: log.Std.Tee(nil, "executor: "),
	}
	if err := p.Start(); err != nil {
		return err
	}
	if s.EC2Cluster {
		go func() {
			const (
				period = time.Minute
				expiry = 10 * time.Minute
			)
			// Always give the instance an expiry period to receive work,
			// then check periodically if the instance has been idle for more
			// than the expiry time.
			time.Sleep(expiry)
			for {
				if p.StopIfIdleFor(expiry) {
					log.Fatalf("reflowlet idle for %s; shutting down", expiry)
				}
				time.Sleep(period)
			}
		}()
	}

	var httpLog *log.Logger
	if s.HTTPDebug {
		httpLog = log.Std.Tee(nil, "http: ")
		httpLog.Level = log.DebugLevel
		log.Std.Level = log.DebugLevel
	}

	http.Handle("/", rest.Handler(server.NewNode(p), httpLog))
	// Add the reflowlet version to the config and serve it from an API.
	cfgNode, err := newConfigNode(s.Config)
	if err != nil {
		return fmt.Errorf("read config: %v", err)
	}
	http.Handle("/v1/config", rest.DoFuncHandler(cfgNode, httpLog))
	var repo reflow.Repository
	err = s.Config.Instance(&repo)
	if err != nil {
		return fmt.Errorf("repo: %v", err)
	}
	http.Handle("/v1/execimage", rest.DoFuncHandler(newExecImageNode(p, repo), httpLog))
	server := &http.Server{Addr: s.Addr}
	if s.Insecure {
		return server.ListenAndServe()
	}
	serverConfig.ClientAuth = tls.RequireAndVerifyClientCert
	server.TLSConfig = serverConfig
	http2.ConfigureServer(server, &http2.Server{
		MaxConcurrentStreams: maxConcurrentStreams,
	})
	return server.ListenAndServeTLS("", "")
}

// IgnoreSigpipe consumes (and ignores) SIGPIPE signals. As of Go
// 1.6, these are generated only for stdout and stderr.
//
// This is useful where a reflowlet's standard output is closed while
// running, as can happen when journald restarts on systemd managed
// systems.
//
// See the following for more information:
//	https://bugzilla.redhat.com/show_bug.cgi?id=1300076
func IgnoreSigpipe() {
	c := make(chan os.Signal, 1024)
	signal.Notify(c, os.Signal(syscall.SIGPIPE))
	for {
		<-c
	}
}

func newConfigNode(cfg infra.Config) (rest.DoFunc, error) {
	b, err := cfg.Marshal(false)
	if err != nil {
		return nil, fmt.Errorf("serialize keys: %v", err)
	}
	return func(ctx context.Context, call *rest.Call) {
		if !call.Allow("GET") {
			return
		}
		call.Reply(http.StatusOK, string(b))
	}, nil
}

func newExecImageNode(p *local.Pool, repo reflow.Repository) rest.DoFunc {
	return rest.DoFunc(func(ctx context.Context, call *rest.Call) {
		if !call.Allow("GET", "POST") {
			return
		}
		switch call.Method() {
		case "GET":
			dig, err := execimage.ImageDigest()
			if err != nil {
				call.Error(fmt.Errorf("execimage GET: %v", err))
				return
			}
			call.Reply(http.StatusOK, dig)
		case "POST":
			stopped := p.StopIfIdleFor(0)
			if !stopped {
				call.Error(errors.New("reflowlet not idle"))
				return
			}
			var d digest.Digest
			if err := call.Unmarshal(&d); err != nil {
				return
			}
			image, err := repo.Get(ctx, d)
			if err != nil {
				call.Error(fmt.Errorf("execimage POST: %v", err))
				return
			}
			if err := execimage.InstallImageReflowlet(image, "reflowlet"+d.HexN(7)); err != nil {
				call.Error(fmt.Errorf("execimage POST: %v", err))
				return
			}
			panic("should never reach")
		}
	})
}
