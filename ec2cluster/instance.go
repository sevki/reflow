// Copyright 2017 GRAIL, Inc. All rights reserved.
// Use of this source code is governed by the Apache 2.0
// license that can be found in the LICENSE file.

package ec2cluster

//go:generate go run ../cmd/ec2instances/main.go instances

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"text/template"
	"time"
	"unicode"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ec2/ec2iface"
	"github.com/grailbio/base/digest"
	"github.com/grailbio/base/status"
	"github.com/grailbio/base/sync/once"
	"github.com/grailbio/infra"
	"github.com/grailbio/reflow"
	"github.com/grailbio/reflow/ec2cluster/instances"
	"github.com/grailbio/reflow/errors"
	infra2 "github.com/grailbio/reflow/infra"
	"github.com/grailbio/reflow/internal/ecrauth"
	"github.com/grailbio/reflow/internal/execimage"
	"github.com/grailbio/reflow/log"
	"github.com/grailbio/reflow/pool"
	"github.com/grailbio/reflow/pool/client"
	"gopkg.in/yaml.v2"
)

// memoryDiscount is the amount of memory that's reserved by the
// reflowlet and EC2 together. Ideally we'd be able to be more
// precise about this (e.g., extract actual guarantees about
// available memory from AWS), but this doesn't seem possible at this
// time.
//
// We reserve 5% for the Reflowlet, and the EC2 overhead appears to
// be a little shy of 2%.
const memoryDiscount = 0.05 + 0.02

const (
	// ebsThroughputPremiumCost defines the higher premium in USD dollars
	// we are willing to pay for an instance with at least ebsThroughputBenefitPct more EBS throughput
	ebsThroughputPremiumCost = 0.03

	// ebsThroughputPremiumPct defines the higher premium (as a percentage)
	// we are willing to pay for at least ebsThroughputBenefitPct increased EBS throughput
	// when choosing instance types.
	ebsThroughputPremiumPct = 15.0

	// ebsThroughputBenefitPct is the percentage higher EBS throughput we require
	// to justify paying the premium.
	ebsThroughputBenefitPct = 50.0
)

// the smallest acceptable disk sizes per EBS volume type.
var minDiskSizes = map[string]uint64{
	// EBS does not allow you to create ST1 volumes smaller than 500GiB.
	"st1": 500,
	// 334GiB is the smallest disk size that yields maximum throughput, as per
	// https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/EBSVolumeTypes.html
	"gp2": 335,
}

// instanceConfig represents a instance configuration.
type instanceConfig struct {
	// Type is the EC2 instance type to be launched.
	Type string

	// EBSOptimized is true if we should request an EBS optimized instance.
	EBSOptimized bool
	// EBSThroughput is the max throughput for the EBS optimized instance.
	EBSThroughput float64
	// Resources holds the Reflow resources that are presented by this configuration.
	// It does not include disk sizes; they are dynamic.
	Resources reflow.Resources
	// Price is the on-demand price for this instance type in fractional dollars, in available regions.
	Price map[string]float64
	// SpotOk tells whether spot is supported for this instance type.
	SpotOk bool
	// NVMe specifies whether EBS is exposed as NVMe devices.
	NVMe bool
}

var (
	instanceTypes = map[string]instanceConfig{}
	localDigest   digest.Digest
	digestOnce    once.Task
	uploadOnce    once.Task
)

func init() {
	for _, typ := range instances.Types {
		instanceTypes[typ.Name] = instanceConfig{
			Type:          typ.Name,
			EBSOptimized:  typ.EBSOptimized,
			EBSThroughput: typ.EBSThroughput,
			Price:         typ.Price,
			Resources: reflow.Resources{
				"cpu": float64(typ.VCPU),
				"mem": (1 - memoryDiscount) * typ.Memory * 1024 * 1024 * 1024,
			},
			// According to Amazon, "t2" instances are the only current-generation
			// instances not supported by spot.
			SpotOk: typ.Generation == "current" && !strings.HasPrefix(typ.Name, "t2."),
			NVMe:   typ.NVMe,
		}
		for key, ok := range typ.CPUFeatures {
			if !ok {
				continue
			}
			// Allocate one feature per VCPU.
			instanceTypes[typ.Name].Resources[key] = float64(typ.VCPU)
		}
	}
}

// instanceState stores everything we know about EC2 instances,
// and implements instance type selection according to runtime
// criteria.
type instanceState struct {
	configs   []instanceConfig
	sleepTime time.Duration
	region    string

	mu          sync.Mutex
	unavailable map[string]time.Time
}

func newInstanceState(configs []instanceConfig, sleep time.Duration, region string) *instanceState {
	s := &instanceState{
		configs:     make([]instanceConfig, len(configs)),
		unavailable: make(map[string]time.Time),
		sleepTime:   sleep,
		region:      region,
	}
	copy(s.configs, configs)
	sort.Slice(s.configs, func(i, j int) bool {
		return s.configs[j].Resources["mem"] < s.configs[i].Resources["mem"]
	})
	return s
}

// Unavailable marks the given instance config as busy.
func (s *instanceState) Unavailable(config instanceConfig) {
	s.mu.Lock()
	s.unavailable[config.Type] = time.Now()
	s.mu.Unlock()
}

// Available tells whether the provided resources are potentially
// available as an EC2 instance.
func (s *instanceState) Available(need reflow.Resources) bool {
	for _, config := range s.configs {
		if config.Resources.Available(need) {
			return true
		}
	}
	return false
}

// MaxAvailable returns the "largest" instance type that has at least
// the required resources and is also believed to be currently
// available. Spot restricts instances to those that may be launched
// via EC2 spot market. MaxAvailable uses (Resources).ScoredDistance
// to determine the largest instance type.
func (s *instanceState) MaxAvailable(need reflow.Resources, spot bool) (instanceConfig, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var (
		best     instanceConfig
		distance float64 = -math.MaxFloat64
	)
	for _, config := range s.configs {
		if time.Since(s.unavailable[config.Type]) < s.sleepTime || (spot && !config.SpotOk) {
			continue
		}
		if !config.Resources.Available(need) {
			continue
		}
		if d := config.Resources.ScaledDistance(need); d > distance {
			distance = d
			best = config
		}
	}
	return best, best.Resources.Available(need)
}

// MinAvailable returns the cheapest instance type that has at least
// the required resources and is also believed to be currently
// available. Spot restricts instances to those that may be launched
// via EC2 spot market.
func (s *instanceState) MinAvailable(need reflow.Resources, spot bool) (instanceConfig, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var (
		price     float64
		best      instanceConfig
		bestPrice = math.MaxFloat64
		found, ok bool
		viable    []instanceConfig
	)
	for _, config := range s.configs {
		if time.Since(s.unavailable[config.Type]) < s.sleepTime || (spot && !config.SpotOk) {
			continue
		}
		if !config.Resources.Available(need) {
			continue
		}
		if price, ok = config.Price[s.region]; !ok {
			continue
		}
		viable = append(viable, config)
		if price < bestPrice {
			bestPrice = price
			best = config
		}
	}
	// Choose a higher cost but better EBS throughput instance type if applicable.
	for _, config := range viable {
		price = config.Price[s.region]
		// Prefer a reasonably more expensive one with higher EBS throughput
		if !found &&
			(price < bestPrice+ebsThroughputPremiumCost ||
				price < bestPrice*(1.0+ebsThroughputPremiumPct/100)) &&
			config.EBSThroughput > best.EBSThroughput*(1.0+ebsThroughputBenefitPct/100) {
			bestPrice = price
			best = config
			found = true
		}
		// Prefer a cheaper one with same EBS throughput.
		if found && price < bestPrice && config.EBSThroughput >= best.EBSThroughput {
			bestPrice = price
			best = config
		}
	}
	return best, best.Resources.Available(need)
}

func (s *instanceState) Type(typ string) (instanceConfig, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Since(s.unavailable[typ]) < s.sleepTime {
		return instanceConfig{}, false
	}
	for _, config := range s.configs {
		if config.Type == typ {
			return config, true
		}
	}
	return instanceConfig{}, false
}

// instance represents a concrete instance; it is launched from an instanceConfig
// and additional parameters.
type instance struct {
	HTTPClient      *http.Client
	Config          instanceConfig
	ReflowConfig    infra.Config
	Log             *log.Logger
	Authenticator   ecrauth.Interface
	EC2             ec2iface.EC2API
	InstanceTags    map[string]string
	Labels          pool.Labels
	Spot            bool
	Subnet          string
	InstanceProfile string
	SecurityGroup   string
	Region          string
	ReflowletImage  string
	Price           float64
	EBSType         string
	EBSSize         uint64
	NEBS            int
	AMI             string
	KeyName         string
	SpotProbeDepth  int
	SshKey          string
	Immortal        bool
	CloudConfig     cloudConfig
	Task            *status.Task

	userData string
	err      error
	ec2inst  *ec2.Instance
}

type reflowletInstance struct {
	ec2.Instance

	// Version of the reflowlet instance running on EC2 (populated on instance startup)
	Version string
	// Digest of the executable running on the reflowlet instance
	Digest string
}

func newReflowletInstance(inst *ec2.Instance) *reflowletInstance {
	ri := &reflowletInstance{Instance: *inst}
	ri.update()
	return ri
}

func (i *reflowletInstance) update() {
	for _, tag := range i.Instance.Tags {
		if *tag.Key == "reflowlet:version" {
			i.Version = *tag.Value
		}
		if *tag.Key == "reflowlet:digest" {
			i.Digest = *tag.Value
		}
		if i.Digest != "" && i.Version != "" {
			break
		}
	}
}

// Err returns any error that occurred while launching the instance.
func (i *instance) Err() error {
	return i.err
}

// Instance returns the EC2 instance metadata returned by a successful launch.
func (i *instance) Instance() *reflowletInstance {
	return newReflowletInstance(i.ec2inst)
}

// Go launches an instance, and returns when it fails or the context is done.
// On success (i.Err() == nil), the returned instance is in running state.
// Launch status is reported to the instance's task, if any.
func (i *instance) Go(ctx context.Context) {
	if i.NEBS < 1 {
		i.NEBS = 1
	}
	if min, ok := minDiskSizes[i.EBSType]; ok {
		if i.EBSSize < min {
			i.EBSSize = min
		}
		nmin := int(i.EBSSize / min)
		if i.NEBS > nmin {
			i.NEBS = nmin
		}
	}
	const maxTries = 5
	type stateT int
	const (
		// Perform capacity check for EC2 spot.
		stateCapacity stateT = iota
		// Launch the instance via EC2.
		stateLaunch
		// Tag the instance
		stateTag
		// Wait for the instance to enter running state.
		stateWaitInstance
		// Describe the instance via EC2 to get the DNS name.
		stateDescribeDns
		// Wait for Reflowlet to become live (and metadata to become available).
		stateWaitReflowlet
		// Describe the instance via EC2 to get updated tags for version and digest.
		stateDescribeTags
		// Update the reflowlet image.
		stateUpdateImage
		stateDone
	)
	var (
		state stateT
		id    string
		dns   string
		n     int
		d     = 5 * time.Second
	)
	// TODO(marius): propagate context to the underlying AWS calls
	for state < stateDone && ctx.Err() == nil {
		switch state {
		case stateCapacity:
			if !i.Spot || i.SpotProbeDepth == 0 {
				break
			}
			i.Task.Print("probing for EC2 capacity")
			// 20 instances should be a good margin for spot.
			var ok bool
			ok, i.err = i.ec2HasCapacity(ctx, i.SpotProbeDepth)
			if i.err == nil && !ok {
				i.err = errors.E(errors.Unavailable, errors.New("ec2 capacity is likely exhausted"))
			}
		case stateLaunch:
			i.Task.Print("launching EC2 instance")
			id, i.err = i.launch(ctx)
			if i.err != nil {
				i.Task.Printf("launch error: %v", i.err)
				i.Log.Errorf("instance launch error: %v", i.err)
			} else {
				spot := ""
				if i.Spot {
					spot = "spot "
				}
				i.Task.Title(id)
				i.Task.Print("launched")
				i.Log.Debugf("launched %sinstance %v: %s%s", spot, id, i.Config.Type, i.Config.Resources)
			}
		case stateTag:
			var tags []*ec2.Tag
			for k, v := range i.InstanceTags {
				tags = append(tags, &ec2.Tag{Key: aws.String(k), Value: aws.String(v)})
			}
			for k, v := range i.Labels {
				tags = append(tags, &ec2.Tag{Key: aws.String(k), Value: aws.String(v)})
			}
			_, i.err = i.EC2.CreateTags(&ec2.CreateTagsInput{Resources: []*string{aws.String(id)}, Tags: tags})
		case stateWaitInstance:
			i.Task.Print("waiting for instance to become ready")
			i.err = i.EC2.WaitUntilInstanceRunning(&ec2.DescribeInstancesInput{
				InstanceIds: []*string{aws.String(id)},
			})
		case stateDescribeDns:
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			i.ec2inst, i.err = describeInstance(ctx, i.EC2, id)
			cancel()
			if i.err == nil {
				if i.ec2inst.PublicDnsName == nil || *i.ec2inst.PublicDnsName == "" {
					i.err = errors.Errorf("ec2.describeinstances %v: no public DNS name", id)
				} else {
					dns = *i.ec2inst.PublicDnsName
				}
			}
		case stateWaitReflowlet:
			i.Task.Print("waiting for reflowlet to become available")
			var c *client.Client
			c, i.err = client.New(fmt.Sprintf("https://%s:9000/v1/", dns), i.HTTPClient, nil /*log.New(os.Stderr, "client: ", 0)*/)
			if i.err != nil {
				i.err = errors.E(errors.Fatal, i.err)
				break
			}
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			_, i.err = c.Config(ctx)
			cancel()
			if i.err != nil && strings.HasSuffix(i.err.Error(), "connection refused") {
				i.err = errors.E(errors.Temporary, i.err)
			}
		case stateDescribeTags:
			i.Task.Print("waiting for reflowlet version/digest tags")
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			i.ec2inst, i.err = describeInstance(ctx, i.EC2, id)
			cancel()
			if i.err != nil {
				i.err = errors.E(errors.Temporary, "%s: describe instance: %v", id)
				break
			}
			ri := i.Instance()
			if ri.Version == "" || ri.Digest == "" {
				i.err = errors.E(errors.Temporary, "version/digest unavailable")
				break
			}
			localDigest, err := imageDigest()
			if err != nil {
				i.err = errors.E(errors.Fatal, "parse local digest: %v", err)
				break
			}
			remoteDigest, err := digest.Parse(ri.Digest)
			if err != nil {
				i.err = errors.E(errors.Fatal, "parse remote digest: %v", err)
				break
			}
			if remoteDigest == localDigest {
				// Set the state to be done.
				state = stateDone
			} else {
				i.Log.Debugf("%s: image %s needs update %s", id, remoteDigest.Short(), localDigest.Short())
			}
		case stateUpdateImage:
			i.Task.Print("updating reflowlet image")
			clnt, err := client.New(fmt.Sprintf("https://%s:9000/v1/", *i.ec2inst.PublicDnsName), i.HTTPClient, nil)
			if err != nil {
				i.err = errors.E(errors.Fatal, err)
				break
			}
			var repo reflow.Repository
			err = i.ReflowConfig.Instance(&repo)
			if err != nil {
				i.err = errors.E(errors.Fatal, err)
				break
			}
			ctx2, cancel := context.WithTimeout(ctx, 5*time.Minute)
			err = uploadImage(ctx2, repo, i.Log)
			cancel()
			if err != nil {
				i.err = errors.E(errors.Fatal, err)
				break
			}
			ctx2, cancel = context.WithTimeout(ctx, 10*time.Second)
			err = clnt.InstallImage(ctx2, localDigest)
			cancel()
			if err != nil && !errors.Is(errors.Net, err) {
				i.err = errors.E(errors.Fatal, err)
				break
			}
			// Reset the state such that the loop continues from stateWaitReflowlet
			state = stateWaitReflowlet
			continue
		default:
			panic("unknown state")
		}
		if i.err == nil {
			n = 0
			d = 5 * time.Second
			state++
			continue
		}
		if n == maxTries {
			break
		}
		if awserr, ok := i.err.(awserr.Error); ok {
			switch awserr.Code() {
			// According to EC2 API docs, these codes indicate
			// capacity issues.
			//
			// http://docs.aws.amazon.com/AWSEC2/latest/APIReference/errors-overview.html
			//
			// TODO(marius): add a separate package for interpreting AWS errors.
			case "InsufficientCapacity", "InsufficientInstanceCapacity", "InsufficientHostCapacity", "InsufficientReservedInstanceCapacity", "InstanceLimitExceeded":
				i.err = errors.E(errors.Unavailable, awserr)
			}
		}
		switch {
		case i.err == nil:
		case errors.Is(errors.Fatal, i.err):
			i.Log.Errorf("instance %v fatal: %v", id, i.err)
			return
		case errors.Is(errors.Unavailable, i.err):
			i.Log.Errorf("instance %v unavailable: %v", id, i.err)
			// Return these immediately because our caller may be able to handle
			// them by selecting a different instance type.
			return
		case !errors.Recover(i.err).Timeout() && !errors.Recover(i.err).Temporary():
			var what string
			switch state {
			case stateCapacity:
				what = "checking capacity"
			case stateLaunch:
				what = "launching instance"
			case stateTag:
				what = "tagging instance"
			case stateWaitInstance:
				what = "waiting for instance"
			case stateDescribeDns:
				what = "describing instance (dns)"
			case stateWaitReflowlet:
				what = "waiting for reflowlet to be live"
			case stateDescribeTags:
				what = "describing instance (version, digest)"
			case stateUpdateImage:
				what = "updating reflowlet image"
			}
			i.Log.Errorf("error while %s: %v", what, i.err)
		}
		time.Sleep(d)
		n++
		d *= time.Duration(2)
		i.Log.Debugf("%s recoverable error (%d/%d): %v", id, n, maxTries, i.err)
	}
	if i.err != nil {
		i.Task.Print(i.err)
		return
	}
	i.err = ctx.Err()
	if i.err == nil {
		i.Task.Print("instance ready")
	}
}

func describeInstance(ctx context.Context, EC2 ec2iface.EC2API, id string) (*ec2.Instance, error) {
	resp, err := EC2.DescribeInstancesWithContext(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []*string{aws.String(id)},
	})
	if err != nil {
		return nil, err
	}
	if len(resp.Reservations) != 1 || len(resp.Reservations[0].Instances) != 1 {
		return nil, errors.Errorf("ec2.describeinstances %v: invalid output", id)
	}
	return resp.Reservations[0].Instances[0], nil
}

func uploadImage(ctx context.Context, repo reflow.Repository, log *log.Logger) error {
	return uploadOnce.Do(func() error {
		if !hasEmbedded() {
			return execimage.ErrNoEmbeddedImage
		}
		localDigest, err := imageDigest()
		if err != nil {
			return err
		}
		if _, err = repo.Stat(ctx, localDigest); err == nil {
			return nil
		}
		// Image doesn't exist in repo, so upload it.
		r, err := execimage.EmbeddedLinuxImage()
		if err != nil {
			return err
		}
		defer r.Close()
		log.Debugf("uploading reflow image (%s) to repo", localDigest.Short())
		repoDigest, err := repo.Put(ctx, r)
		if err != nil {
			return err
		}
		if repoDigest != localDigest {
			return errors.New("digests mismatch")
		}
		return nil
	})
}

func imageDigest() (digest.Digest, error) {
	err := digestOnce.Do(func() error {
		var err error
		r, err := execimage.EmbeddedLinuxImage()
		if err != nil {
			return err
		}
		localDigest, err = execimage.Digest(r)
		defer r.Close()
		return err
	})
	return localDigest, err
}

func hasEmbedded() bool {
	_, err := imageDigest()
	return err == nil
}

func (i *instance) launch(ctx context.Context) (string, error) {
	// First we need to construct the cloud-config that's passed to
	// our instances via EC2's user-data mechanism.
	var c cloudConfig

	if i.SshKey == "" {
		i.Log.Debugf("instance launch: missing public SSH key")
	} else {
		c.SshAuthorizedKeys = []string{i.SshKey}
	}

	// /etc/ecrlogin contains the login command for ECR.
	ecrFile := CloudFile{
		Path:        "/etc/ecrlogin",
		Permissions: "0644",
		Owner:       "root",
	}
	var err error
	ecrFile.Content, err = ecrauth.Login(context.TODO(), i.Authenticator)
	if err != nil {
		return "", err
	}
	c.AppendFile(ecrFile)

	// /etc/reflowconfig contains the (YAML) marshaled configuration file
	// for the reflowlet.
	b, err := i.ReflowConfig.Marshal(true)
	if err != nil {
		return "", err
	}
	// The remote side does not need a cluster implementation.
	keys := make(infra.Keys)
	err = yaml.Unmarshal(b, &keys)
	if err != nil {
		return "", err
	}
	delete(keys, infra2.Cluster)
	b, err = yaml.Marshal(keys)
	if err != nil {
		return "", err
	}
	// Compress file so that we are below the 16KB limit for user data.
	var gb bytes.Buffer
	gw := gzip.NewWriter(&gb)
	_, err = gw.Write(b)
	if err != nil {
		return "", err
	}
	err = gw.Close()
	if err != nil {
		return "", err
	}
	c.AppendFile(CloudFile{
		Path:        "/etc/reflowconfig",
		Permissions: "0644",
		Owner:       "root",
		Encoding:    "gzip",
		Content:     gb.String(),
	})

	// Turn off CoreOS services that would restart or otherwise disrupt
	// the instances.
	c.CoreOS.Update.RebootStrategy = "off"
	c.AppendUnit(CloudUnit{Name: "update-engine.service", Command: "stop"})
	c.AppendUnit(CloudUnit{Name: "locksmithd.service", Command: "stop"})

	// Configure the disks.
	var deviceName string
	switch i.NEBS {
	case 0, 1:
		deviceName = "xvdb"
		if i.Config.NVMe {
			deviceName = "nvme1n1"
		}
		c.AppendUnit(CloudUnit{
			Name:    fmt.Sprintf("format-%s.service", deviceName),
			Command: "start",
			Content: tmpl(`
			[Unit]
			Description=Format /dev/{{.name}}
			After=dev-{{.name}}.device
			Requires=dev-{{.name}}.device
			[Service]
			Type=oneshot
			RemainAfterExit=yes
			ExecStart=/usr/sbin/wipefs -f /dev/{{.name}}
			ExecStart=/usr/sbin/mkfs.ext4 -F /dev/{{.name}}
		`, args{"name": deviceName}),
		})
	default:
		deviceName = "md0"
		devices := make([]string, i.NEBS)
		for idx := range devices {
			if i.Config.NVMe {
				devices[idx] = fmt.Sprintf("nvme%dn1", idx+1)
			} else {
				devices[idx] = fmt.Sprintf("xvd%c", 'b'+idx)
			}
		}
		c.AppendUnit(CloudUnit{
			Name:    fmt.Sprintf("format-%s.service", deviceName),
			Command: "start",
			Content: tmpl(`
			[Unit]
			Description=Format /dev/{{.md}}
			After={{range $_, $name :=  .devices}}dev-{{$name}}.device {{end}}
			Requires={{range $_, $name := .devices}}dev-{{$name}}.device {{end}}
			[Service]
			Type=oneshot
			RemainAfterExit=yes
			ExecStart=/usr/sbin/mdadm --create --run --verbose /dev/{{.md}} --level=0 --chunk=256 --name=reflow --raid-devices={{.devices|len}} {{range $_, $name := .devices}}/dev/{{$name}} {{end}}
			ExecStart=/usr/sbin/mkfs.ext4 -F /dev/{{.md}}
		`, args{"devices": devices, "md": deviceName}),
		})
	}

	c.AppendUnit(CloudUnit{
		Name:    "mnt-data.mount",
		Command: "start",
		Content: tmpl(`
			[Mount]
			What=/dev/{{.name}}
			Where=/mnt/data
			Type=ext4
			Options=data=writeback
		`, args{"name": deviceName}),
	})

	var profile, akey, secret, token string
	if i.InstanceProfile != "" {
		profile = fmt.Sprintf("-a %s", i.InstanceProfile)
	} else {
		var creds *credentials.Credentials
		err := i.ReflowConfig.Instance(&creds)
		if err == nil {
			if c, err := creds.Get(); err == nil {
				akey = fmt.Sprintf("AWS_ACCESS_KEY_ID=%s", c.AccessKeyID)
				secret = fmt.Sprintf("AWS_SECRET_ACCESS_KEY=%s", c.SecretAccessKey)
				token = fmt.Sprintf("AWS_SESSION_TOKEN=%s", c.SessionToken)
			}
		}
	}
	if akey != "" || profile != "" {
		c.AppendUnit(CloudUnit{
			Name:    "aws-xray.service",
			Enable:  true,
			Command: "start",
			Content: tmpl(`
			[Unit]
			Description=xray
			Requires=network.target
			After=network.target
			[Service]
			Environment="{{.aws_access_key_id}}"
			Environment="{{.aws_secret_access_key}}"
			Environment="{{.aws_session_token}}"
			Type=simple
			ExecStartPre=/usr/bin/wget https://s3.dualstack.us-east-2.amazonaws.com/aws-xray-assets.us-east-2/xray-daemon/aws-xray-daemon-linux-2.x.zip
			ExecStartPre=/usr/bin/unzip aws-xray-daemon-linux-2.x.zip -d /tmp
			ExecStart=/tmp/xray {{.profile}} -l debug`, args{"profile": profile, "aws_access_key_id": akey, "aws_secret_access_key": secret, "aws_session_token": token})})
	}

	// We merge the user's cloud config before appending the reflowlet unit
	// so that systemd units can be run before the reflowlet.
	c.Merge(&i.CloudConfig)

	c.AppendUnit(CloudUnit{
		Name:    "reflowlet.service",
		Enable:  true,
		Command: "start",
		Content: tmpl(`
			[Unit]
			Description=reflowlet
			Requires=network.target
			After=network.target
			{{if .mortal}}
			OnFailure=poweroff.target
			OnFailureJobMode=replace-irreversibly
			{{end}}
			[Service]
			OOMScoreAdjust=-1000
			Type=oneshot
			ExecStartPre=-/usr/bin/docker stop %n
			ExecStartPre=-/usr/bin/docker rm %n
			ExecStartPre=/bin/bash /etc/ecrlogin
			ExecStartPre=/usr/bin/docker pull {{.image}}
			ExecStart=/usr/bin/docker run --oom-score-adj -1000 --rm --name %n --net=host \
				-e V23_CREDENTIALS=/host/opt/.v23 \
				-e V23_CREDENTIALS_NO_AGENT=1 \
				-e V23_CREDENTIALS_NO_LOCK=1 \
			  -v /:/host \
			  -v /var/run/docker.sock:/var/run/docker.sock \
			  -v '/etc/ssl/certs/ca-certificates.crt:/etc/ssl/certs/ca-certificates.crt' \
			  {{.image}} serve -prefix /host -ec2cluster  -config /host/etc/reflowconfig
		`, args{"mortal": !i.Immortal, "image": i.ReflowletImage}),
	})
	b, err = c.Marshal()
	if err != nil {
		return "", err
	}
	i.userData = base64.StdEncoding.EncodeToString(b)
	if i.Spot {
		return i.ec2RunSpotInstance(ctx)
	}
	return i.ec2RunInstance()
}

func (i *instance) ec2RunSpotInstance(ctx context.Context) (string, error) {
	i.Log.Debugf("generating ec2 spot instance request for instance type %v", i.Config.Type)
	// First make a spot instance request.
	params := &ec2.RequestSpotInstancesInput{
		ValidUntil: aws.Time(time.Now().Add(time.Minute)),
		SpotPrice:  aws.String(fmt.Sprintf("%.3f", i.Price)),

		LaunchSpecification: &ec2.RequestSpotLaunchSpecification{
			ImageId:             aws.String(i.AMI),
			EbsOptimized:        aws.Bool(i.Config.EBSOptimized),
			InstanceType:        aws.String(i.Config.Type),
			SubnetId:            aws.String(i.Subnet),
			BlockDeviceMappings: i.ebsDeviceMappings(),
			KeyName:             nonemptyString(i.KeyName),
			UserData:            aws.String(i.userData),

			IamInstanceProfile: &ec2.IamInstanceProfileSpecification{
				Arn: aws.String(i.InstanceProfile),
			},
			SecurityGroupIds: []*string{aws.String(i.SecurityGroup)},
		},
	}
	i.Task.Printf("requesting spot instances with bid of %s", *params.SpotPrice)
	resp, err := i.EC2.RequestSpotInstances(params)
	if err != nil {
		return "", err
	}
	if n := len(resp.SpotInstanceRequests); n != 1 {
		return "", errors.Errorf("ec2.requestspotinstances: got %v entries, want 1", n)
	}
	reqid := aws.StringValue(resp.SpotInstanceRequests[0].SpotInstanceRequestId)
	if reqid == "" {
		return "", errors.Errorf("ec2.requestspotinstances: empty request id")
	}
	i.Task.Printf("awaiting fulfillment of spot request %s", reqid)
	i.Log.Debugf("waiting for spot fullfillment for instance type %v: %s", i.Config.Type, reqid)
	// Also set a timeout context in case the AWS API is stuck.
	toctx, cancel := context.WithTimeout(ctx, time.Minute+10*time.Second)
	defer cancel()
	if err := i.ec2WaitForSpotFulfillment(toctx, reqid); err != nil {
		// If we're not fulfilled by our deadline, we consider spot instances
		// unavailable. Boot this up to the caller so they can pick a different
		// instance types.
		return "", errors.E(errors.Unavailable, err)
	}
	describe, err := i.EC2.DescribeSpotInstanceRequests(&ec2.DescribeSpotInstanceRequestsInput{
		SpotInstanceRequestIds: []*string{aws.String(reqid)},
	})
	if err != nil {
		return "", err
	}
	if n := len(describe.SpotInstanceRequests); n != 1 {
		return "", errors.Errorf("ec2.describespotinstancerequests: got %v entries, want 1", n)
	}
	id := aws.StringValue(describe.SpotInstanceRequests[0].InstanceId)
	if id == "" {
		return "", errors.Errorf("ec2.describespotinstancerequests: missing instance ID")
	}
	i.Task.Printf("spot request %s fulfilled", reqid)
	i.Log.Debugf("ec2 spot request %s fulfilled", reqid)
	return id, nil
}

// ec2WaitForSpotFulfillment waits until the spot request spotID has been fulfilled.
// It differs from (*ec2.EC2).WaitUntilSpotInstanceRequestFulfilledWithContext
// in that it request-cancelled-and-instance-running as a success.
func (i *instance) ec2WaitForSpotFulfillment(ctx context.Context, spotID string) error {
	w := request.Waiter{
		Name:        "ec2WaitForSpotFulfillment",
		MaxAttempts: 40,                                            // default from SDK
		Delay:       request.ConstantWaiterDelay(15 * time.Second), // default from SDK
		Acceptors: []request.WaiterAcceptor{
			{
				State:   request.SuccessWaiterState,
				Matcher: request.PathAllWaiterMatch, Argument: "SpotInstanceRequests[].Status.Code",
				Expected: "fulfilled",
			},
			{
				State:   request.SuccessWaiterState,
				Matcher: request.PathAllWaiterMatch, Argument: "SpotInstanceRequests[].Status.Code",
				Expected: "request-canceled-and-instance-running",
			},
			{
				State:   request.FailureWaiterState,
				Matcher: request.PathAnyWaiterMatch, Argument: "SpotInstanceRequests[].Status.Code",
				Expected: "schedule-expired",
			},
			{
				State:   request.FailureWaiterState,
				Matcher: request.PathAnyWaiterMatch, Argument: "SpotInstanceRequests[].Status.Code",
				Expected: "canceled-before-fulfillment",
			},
			{
				State:   request.FailureWaiterState,
				Matcher: request.PathAnyWaiterMatch, Argument: "SpotInstanceRequests[].Status.Code",
				Expected: "bad-parameters",
			},
			{
				State:   request.FailureWaiterState,
				Matcher: request.PathAnyWaiterMatch, Argument: "SpotInstanceRequests[].Status.Code",
				Expected: "system-error",
			},
		},
		NewRequest: func(opts []request.Option) (*request.Request, error) {
			req, _ := i.EC2.DescribeSpotInstanceRequestsRequest(&ec2.DescribeSpotInstanceRequestsInput{
				SpotInstanceRequestIds: []*string{aws.String(spotID)},
			})
			req.SetContext(ctx)
			req.ApplyOptions(opts...)
			return req, nil
		},
	}
	return w.WaitWithContext(ctx)
}

func (i *instance) ec2HasCapacity(ctx context.Context, n int) (bool, error) {
	params := &ec2.RunInstancesInput{
		DryRun:       aws.Bool(true),
		MinCount:     aws.Int64(int64(n)),
		MaxCount:     aws.Int64(int64(n)),
		ImageId:      aws.String(i.AMI),
		InstanceType: aws.String(i.Config.Type),
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	_, err := i.EC2.RunInstancesWithContext(ctx, params)
	if err == nil {
		return false, errors.New("did not expect successful response")
	} else if awserr, ok := err.(awserr.Error); ok {
		switch awserr.Code() {
		case "DryRunOperation":
			return true, nil
		case "RequestCanceled":
			// Apparently AWS's Go SDK will return an AWS error even for
			// context errors. In this case, we treat a timeout as a negative
			// answer.
			return false, nil
		}
		return false, awserr
	} else if err == context.DeadlineExceeded {
		// We'll take an API timeout as a negative answer: this seems to
		// the case empirically.
		return false, nil
	} else if err := ctx.Err(); err != nil {
		return false, err
	}
	return false, fmt.Errorf("expected awserr.Error or context error, got %T", err)
}

func (i *instance) ec2RunInstance() (string, error) {
	params := &ec2.RunInstancesInput{
		ImageId:               aws.String(i.AMI),
		MaxCount:              aws.Int64(int64(1)),
		MinCount:              aws.Int64(int64(1)),
		BlockDeviceMappings:   i.ebsDeviceMappings(),
		ClientToken:           aws.String(newID()),
		DisableApiTermination: aws.Bool(false),
		DryRun:                aws.Bool(false),
		EbsOptimized:          aws.Bool(i.Config.EBSOptimized),
		IamInstanceProfile: &ec2.IamInstanceProfileSpecification{
			Arn: aws.String(i.InstanceProfile),
		},
		InstanceInitiatedShutdownBehavior: aws.String("terminate"),
		InstanceType:                      aws.String(i.Config.Type),
		Monitoring: &ec2.RunInstancesMonitoringEnabled{
			Enabled: aws.Bool(true), // Required
		},
		KeyName:          nonemptyString(i.KeyName),
		UserData:         aws.String(i.userData),
		SecurityGroupIds: []*string{aws.String(i.SecurityGroup)},
		SubnetId:         aws.String(i.Subnet),
	}
	i.Log.Debugf("EC2RunInstances %v", params)
	resv, err := i.EC2.RunInstances(params)
	if err != nil {
		return "", err
	}
	if n := len(resv.Instances); n != 1 {
		return "", fmt.Errorf("expected 1 instance; got %d", n)
	}
	return *resv.Instances[0].InstanceId, nil
}

// ebsDeviceMappings returns the set of device mappings requested by
// this instance. When i.NEBS > 1, it requests multiple devices which
// are then RAIDed together. We assume that the first mapping,
// device xvda is reserved as a system device.
func (i *instance) ebsDeviceMappings() []*ec2.BlockDeviceMapping {
	mappings := []*ec2.BlockDeviceMapping{
		{
			// The root device for the OS, Docker images, etc.
			DeviceName: aws.String("/dev/xvda"),
			Ebs: &ec2.EbsBlockDevice{
				DeleteOnTermination: aws.Bool(true),
				VolumeSize:          aws.Int64(200),
				VolumeType:          aws.String("gp2"),
			},
		},
	}
	for idx := 0; idx < i.NEBS; idx++ {
		mappings = append(mappings, &ec2.BlockDeviceMapping{
			DeviceName: aws.String(fmt.Sprintf("/dev/xvd%c", 'b'+idx)),
			Ebs: &ec2.EbsBlockDevice{
				DeleteOnTermination: aws.Bool(true),
				VolumeSize:          aws.Int64(int64(i.EBSSize) / int64(i.NEBS)),
				VolumeType:          aws.String(i.EBSType),
			},
		})
	}
	return mappings
}

func newID() string {
	var b [8]byte
	_, err := rand.Read(b[:])
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf("%x", b[:])
}

// nonemptyString returns nil if s is empty, or else the pointer to s.
func nonemptyString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

type args map[string]interface{}

// tmpl renders the template text, after first stripping common
// (whitespace) prefixes from text.
func tmpl(text string, args interface{}) string {
	lines := strings.Split(text, "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	var p int
	if len(lines) > 0 {
		p = strings.IndexFunc(lines[0], func(r rune) bool { return !unicode.IsSpace(r) })
		if p < 0 {
			p = 0
		}
	}
	for i, line := range lines {
		lines[i] = line[p:]
		if strings.TrimSpace(line[:p]) != "" {
			panic(fmt.Sprintf("nonspace prefix in %q", line))
		}
	}
	text = strings.Join(lines, "\n")
	t := template.Must(template.New("ec2template").Parse(text))
	var b bytes.Buffer
	if err := t.Execute(&b, args); err != nil {
		panic(err)
	}
	return b.String()
}
