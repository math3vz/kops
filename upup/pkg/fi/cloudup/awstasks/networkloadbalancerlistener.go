/*
Copyright 2019 The Kubernetes Authors.

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

package awstasks

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	elbv2 "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	elbv2types "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"
	"k8s.io/klog/v2"
	"k8s.io/kops/upup/pkg/fi"
	"k8s.io/kops/upup/pkg/fi/cloudup/awsup"
	"k8s.io/kops/upup/pkg/fi/cloudup/terraform"
	"k8s.io/kops/upup/pkg/fi/cloudup/terraformWriter"
)

// +kops:fitask
type NetworkLoadBalancerListener struct {
	// We use the Name tag to find the existing NLB, because we are (more or less) unrestricted when
	// it comes to tag values, but the LoadBalancerName is length limited
	Name      *string
	Lifecycle fi.Lifecycle

	NetworkLoadBalancer *NetworkLoadBalancer

	Port             int
	TargetGroup      *TargetGroup
	SSLCertificateID string
	SSLPolicy        string

	listenerArn string
}

var _ fi.CompareWithID = &NetworkLoadBalancerListener{}
var _ fi.CloudupTaskNormalize = &NetworkLoadBalancerListener{}

func (e *NetworkLoadBalancerListener) CompareWithID() *string {
	return e.Name
}

func (e *NetworkLoadBalancerListener) Find(c *fi.CloudupContext) (*NetworkLoadBalancerListener, error) {
	ctx := c.Context()

	cloud := c.T.Cloud.(awsup.AWSCloud)

	if e.NetworkLoadBalancer == nil {
		return nil, fi.RequiredField("NetworkLoadBalancer")
	}

	loadBalancerArn := e.NetworkLoadBalancer.loadBalancerArn
	if loadBalancerArn == "" {
		return nil, nil
	}

	var l *elbv2types.Listener
	{
		request := &elbv2.DescribeListenersInput{
			LoadBalancerArn: &loadBalancerArn,
		}
		// TODO: Move to lbInfo?
		var allListeners []elbv2types.Listener
		paginator := elbv2.NewDescribeListenersPaginator(cloud.ELBV2(), request)
		for paginator.HasMorePages() {
			page, err := paginator.NextPage(ctx)
			if err != nil {
				return nil, fmt.Errorf("error querying for NLB listeners :%v", err)
			}
			allListeners = append(allListeners, page.Listeners...)
		}

		var matches []elbv2types.Listener
		for _, listener := range allListeners {
			if aws.ToInt32(listener.Port) == int32(e.Port) {
				matches = append(matches, listener)
			}
		}
		if len(matches) == 0 {
			return nil, nil
		}
		if len(matches) > 1 {
			return nil, fmt.Errorf("found multiple listeners matching %+v", e)
		}
		l = &matches[0]
	}

	actual := &NetworkLoadBalancerListener{}
	actual.listenerArn = aws.ToString(l.ListenerArn)

	actual.Port = int(aws.ToInt32(l.Port))
	if len(l.Certificates) != 0 {
		actual.SSLCertificateID = aws.ToString(l.Certificates[0].CertificateArn) // What if there is more then one certificate, can we just grab the default certificate? we don't set it as default, we only set the one.
		if l.SslPolicy != nil {
			actual.SSLPolicy = aws.ToString(l.SslPolicy)
		}
	}

	// This will need to be rearranged when we recognized multiple listeners and target groups per NLB
	if len(l.DefaultActions) > 0 {
		targetGroupARN := l.DefaultActions[0].TargetGroupArn
		if targetGroupARN != nil {
			actual.TargetGroup = &TargetGroup{
				ARN: targetGroupARN,
			}
		}
	}

	_ = actual.Normalize(c)
	actual.Lifecycle = e.Lifecycle

	// Avoid spurious changes
	actual.Name = e.Name
	actual.NetworkLoadBalancer = e.NetworkLoadBalancer

	klog.V(4).Infof("Found NLB listener %+v", actual)

	return actual, nil
}

func (e *NetworkLoadBalancerListener) Run(c *fi.CloudupContext) error {
	return fi.CloudupDefaultDeltaRunMethod(e, c)
}

func (e *NetworkLoadBalancerListener) Normalize(c *fi.CloudupContext) error {
	return nil
}

func (*NetworkLoadBalancerListener) CheckChanges(a, e, changes *NetworkLoadBalancerListener) error {
	return nil
}

func (*NetworkLoadBalancerListener) RenderAWS(t *awsup.AWSAPITarget, a, e, changes *NetworkLoadBalancerListener) error {
	ctx := context.TODO()

	if e.NetworkLoadBalancer == nil {
		return fi.RequiredField("NetworkLoadBalancer")
	}
	loadBalancerArn := e.NetworkLoadBalancer.loadBalancerArn
	if loadBalancerArn == "" {
		return fmt.Errorf("load balancer not yet created (arn not set)")
	}

	if a != nil {
		// TODO: Can we do better here?
		klog.Warningf("deleting ELB listener %q for required changes (%+v)", a.listenerArn, changes)

		// delete the listener before recreating it
		_, err := t.Cloud.ELBV2().DeleteListener(ctx, &elbv2.DeleteListenerInput{
			ListenerArn: &a.listenerArn,
		})
		if err != nil {
			return fmt.Errorf("error deleting load balancer listener with arn=%q: %w", e.listenerArn, err)
		}
		a = nil
	}

	if a == nil {
		if e.TargetGroup == nil {
			return fi.RequiredField("TargetGroup")
		}
		targetGroupARN := fi.ValueOf(e.TargetGroup.ARN)
		if targetGroupARN == "" {
			return fmt.Errorf("target group not yet created (arn not set)")
		}
		request := &elbv2.CreateListenerInput{
			DefaultActions: []elbv2types.Action{
				{
					TargetGroupArn: aws.String(targetGroupARN),
					Type:           elbv2types.ActionTypeEnumForward,
				},
			},
			LoadBalancerArn: aws.String(loadBalancerArn),
			Port:            aws.Int32(int32(e.Port)),
		}

		if e.SSLCertificateID != "" {
			request.Certificates = []elbv2types.Certificate{}
			request.Certificates = append(request.Certificates, elbv2types.Certificate{
				CertificateArn: aws.String(e.SSLCertificateID),
			})
			request.Protocol = elbv2types.ProtocolEnumTls
			if e.SSLPolicy != "" {
				request.SslPolicy = aws.String(e.SSLPolicy)
			}
		} else {
			request.Protocol = elbv2types.ProtocolEnumTcp
		}

		klog.V(2).Infof("Creating Listener for NLB with port %v", e.Port)
		_, err := t.Cloud.ELBV2().CreateListener(ctx, request)
		if err != nil {
			return fmt.Errorf("creating listener for NLB on port %v: %w", e.Port, err)
		}
	}

	return nil
}

type terraformNetworkLoadBalancerListener struct {
	LoadBalancer   *terraformWriter.Literal                     `cty:"load_balancer_arn"`
	Port           int64                                        `cty:"port"`
	Protocol       elbv2types.ProtocolEnum                      `cty:"protocol"`
	CertificateARN *string                                      `cty:"certificate_arn"`
	SSLPolicy      *string                                      `cty:"ssl_policy"`
	DefaultAction  []terraformNetworkLoadBalancerListenerAction `cty:"default_action"`
}

type terraformNetworkLoadBalancerListenerAction struct {
	Type           elbv2types.ActionTypeEnum `cty:"type"`
	TargetGroupARN *terraformWriter.Literal  `cty:"target_group_arn"`
}

func (_ *NetworkLoadBalancerListener) RenderTerraform(t *terraform.TerraformTarget, a, e, changes *NetworkLoadBalancerListener) error {
	if e.TargetGroup == nil {
		return fi.RequiredField("TargetGroup")
	}
	listenerTF := &terraformNetworkLoadBalancerListener{
		LoadBalancer: e.NetworkLoadBalancer.TerraformLink(),
		Port:         int64(e.Port),
		DefaultAction: []terraformNetworkLoadBalancerListenerAction{
			{
				Type:           elbv2types.ActionTypeEnumForward,
				TargetGroupARN: e.TargetGroup.TerraformLink(),
			},
		},
	}
	if e.SSLCertificateID != "" {
		listenerTF.CertificateARN = &e.SSLCertificateID
		listenerTF.Protocol = elbv2types.ProtocolEnumTls
		if e.SSLPolicy != "" {
			listenerTF.SSLPolicy = &e.SSLPolicy
		}
	} else {
		listenerTF.Protocol = elbv2types.ProtocolEnumTcp
	}

	err := t.RenderResource("aws_lb_listener", e.TerraformName(), listenerTF)
	if err != nil {
		return err
	}

	return nil
}

func (e *NetworkLoadBalancerListener) TerraformName() string {
	tfName := fmt.Sprintf("%v-%v", e.NetworkLoadBalancer.TerraformName(), e.Port)
	return tfName
}
