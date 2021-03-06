/*
Copyright 2020 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cloudup

import (
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog"
	"k8s.io/kops"
	api "k8s.io/kops/pkg/apis/kops"
	"k8s.io/kops/pkg/client/simple"
	"k8s.io/kops/upup/pkg/fi"
	"k8s.io/kops/upup/pkg/fi/cloudup/awsup"
	"k8s.io/kops/upup/pkg/fi/cloudup/openstack"
)

const (
	AuthorizationFlagAlwaysAllow = "AlwaysAllow"
	AuthorizationFlagRBAC        = "RBAC"
)

type NewClusterOptions struct {
	// ClusterName is the name of the cluster to initialize.
	ClusterName string

	// Authorization is the authorization mode to use. The options are "RBAC" (default) and "AlwaysAllow".
	Authorization string
	// Channel is a channel location for initializing the cluster. It defaults to "stable".
	Channel string
	// ConfigBase is the location where we will store the configuration. It defaults to the state store.
	ConfigBase string

	// CloudProvider is the name of the cloud provider. The default is to guess based on the Zones name.
	CloudProvider string
	// Zones are the availability zones in which to run the cluster.
	Zones []string
	// MasterZones are the availability zones in which to run the masters. Defaults to the list in the Zones field.
	MasterZones []string

	// NetworkID is the ID of the shared network (VPC).
	// If empty, SubnetIDs are not empty, and on AWS or OpenStack, determines network ID from the first SubnetID.
	// If empty otherwise, creates a new network/VPC to be owned by the cluster.
	NetworkID string
	// SubnetIDs are the IDs of the shared subnets. If empty, creates new subnets to be owned by the cluster.
	SubnetIDs []string

	// OpenstackExternalNet is the name of the external network for the openstack router
	OpenstackExternalNet     string
	OpenstackExternalSubnet  string
	OpenstackStorageIgnoreAZ bool
	OpenstackDNSServers      string
	OpenstackLbSubnet        string
	// OpenstackLBOctavia is boolean value should we use octavia or old loadbalancer api
	OpenstackLBOctavia bool
}

func (o *NewClusterOptions) InitDefaults() {
	o.Channel = api.DefaultChannel
	o.Authorization = AuthorizationFlagRBAC
}

type NewClusterResult struct {
	// Cluster is the initialized Cluster resource.
	Cluster *api.Cluster

	// TODO remove after more create_cluster logic refactored in
	Channel  *api.Channel
	AllZones sets.String
}

// NewCluster initializes cluster and instance groups specifications as
// intended for newly created clusters.
func NewCluster(opt *NewClusterOptions, clientset simple.Clientset) (*NewClusterResult, error) {
	if opt.ClusterName == "" {
		return nil, fmt.Errorf("name is required")
	}

	if opt.Channel == "" {
		opt.Channel = api.DefaultChannel
	}
	channel, err := api.LoadChannel(opt.Channel)
	if err != nil {
		return nil, err
	}

	cluster := api.Cluster{
		ObjectMeta: v1.ObjectMeta{
			Name: opt.ClusterName,
		},
	}

	if channel.Spec.Cluster != nil {
		cluster.Spec = *channel.Spec.Cluster

		kubernetesVersion := api.RecommendedKubernetesVersion(channel, kops.Version)
		if kubernetesVersion != nil {
			cluster.Spec.KubernetesVersion = kubernetesVersion.String()
		}
	}
	cluster.Spec.Channel = opt.Channel

	cluster.Spec.ConfigBase = opt.ConfigBase
	configBase, err := clientset.ConfigBaseFor(&cluster)
	if err != nil {
		return nil, fmt.Errorf("error building ConfigBase for cluster: %v", err)
	}
	cluster.Spec.ConfigBase = configBase.Path()

	cluster.Spec.Authorization = &api.AuthorizationSpec{}
	if strings.EqualFold(opt.Authorization, AuthorizationFlagAlwaysAllow) {
		cluster.Spec.Authorization.AlwaysAllow = &api.AlwaysAllowAuthorizationSpec{}
	} else if opt.Authorization == "" || strings.EqualFold(opt.Authorization, AuthorizationFlagRBAC) {
		cluster.Spec.Authorization.RBAC = &api.RBACAuthorizationSpec{}
	} else {
		return nil, fmt.Errorf("unknown authorization mode %q", opt.Authorization)
	}

	allZones := sets.NewString()
	allZones.Insert(opt.Zones...)
	allZones.Insert(opt.MasterZones...)

	cluster.Spec.CloudProvider = opt.CloudProvider
	if cluster.Spec.CloudProvider == "" {
		for _, zone := range allZones.List() {
			cloud, known := fi.GuessCloudForZone(zone)
			if known {
				klog.Infof("Inferred %q cloud provider from zone %q", cloud, zone)
				cluster.Spec.CloudProvider = string(cloud)
				break
			}
		}
		if cluster.Spec.CloudProvider == "" {
			if allZones.Len() == 0 {
				return nil, fmt.Errorf("must specify --zones or --cloud")
			}
			return nil, fmt.Errorf("unable to infer cloud provider from zones (is there a typo in --zones?)")
		}
	}

	err = setupVPC(opt, &cluster)
	if err != nil {
		return nil, err
	}

	result := NewClusterResult{
		Cluster:  &cluster,
		Channel:  channel,
		AllZones: allZones,
	}
	return &result, nil
}

func setupVPC(opt *NewClusterOptions, cluster *api.Cluster) error {
	cluster.Spec.NetworkID = opt.NetworkID

	switch api.CloudProviderID(cluster.Spec.CloudProvider) {
	case api.CloudProviderAWS:
		if cluster.Spec.NetworkID == "" && len(opt.SubnetIDs) > 0 {
			cloudTags := map[string]string{}
			awsCloud, err := awsup.NewAWSCloud(opt.Zones[0][:len(opt.Zones[0])-1], cloudTags)
			if err != nil {
				return fmt.Errorf("error loading cloud: %v", err)
			}
			res, err := awsCloud.EC2().DescribeSubnets(&ec2.DescribeSubnetsInput{
				SubnetIds: []*string{aws.String(opt.SubnetIDs[0])},
			})
			if err != nil {
				return fmt.Errorf("error describing subnet %s: %v", opt.SubnetIDs[0], err)
			}
			if len(res.Subnets) == 0 || res.Subnets[0].VpcId == nil {
				return fmt.Errorf("failed to determine VPC id of subnet %s", opt.SubnetIDs[0])
			}
			cluster.Spec.NetworkID = *res.Subnets[0].VpcId
		}

	case api.CloudProviderOpenstack:
		if cluster.Spec.CloudConfig == nil {
			cluster.Spec.CloudConfig = &api.CloudConfiguration{}
		}
		cluster.Spec.CloudConfig.Openstack = &api.OpenstackConfiguration{
			Router: &api.OpenstackRouter{
				ExternalNetwork: fi.String(opt.OpenstackExternalNet),
			},
			BlockStorage: &api.OpenstackBlockStorageConfig{
				Version:  fi.String("v3"),
				IgnoreAZ: fi.Bool(opt.OpenstackStorageIgnoreAZ),
			},
			Monitor: &api.OpenstackMonitor{
				Delay:      fi.String("1m"),
				Timeout:    fi.String("30s"),
				MaxRetries: fi.Int(3),
			},
		}

		if cluster.Spec.NetworkID == "" && len(opt.SubnetIDs) > 0 {
			tags := make(map[string]string)
			tags[openstack.TagClusterName] = opt.ClusterName
			osCloud, err := openstack.NewOpenstackCloud(tags, &cluster.Spec)
			if err != nil {
				return fmt.Errorf("error loading cloud: %v", err)
			}

			res, err := osCloud.FindNetworkBySubnetID(opt.SubnetIDs[0])
			if err != nil {
				return fmt.Errorf("error finding network: %v", err)
			}
			cluster.Spec.NetworkID = res.ID
		}

		if opt.OpenstackDNSServers != "" {
			cluster.Spec.CloudConfig.Openstack.Router.DNSServers = fi.String(opt.OpenstackDNSServers)
		}
		if opt.OpenstackExternalSubnet != "" {
			cluster.Spec.CloudConfig.Openstack.Router.ExternalSubnet = fi.String(opt.OpenstackExternalSubnet)
		}
	}

	return nil
}
