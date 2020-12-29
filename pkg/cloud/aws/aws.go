package aws

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/ec2"

	"github.com/ghodss/yaml"

	"github.com/openshift/library-go/pkg/assets"
	"github.com/openshift/library-go/pkg/operator/events"
	operatorhelpers "github.com/openshift/library-go/pkg/operator/v1helpers"

	workclient "github.com/open-cluster-management/api/client/work/clientset/versioned"
	workv1 "github.com/open-cluster-management/api/work/v1"
	"github.com/open-cluster-management/submariner-addon/pkg/cloud/aws/bindata"
	"github.com/open-cluster-management/submariner-addon/pkg/cloud/aws/client"
	"github.com/open-cluster-management/submariner-addon/pkg/helpers"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

const (
	instanceType              = "m5n.large"
	accessKeyIDSecretKey      = "aws_access_key_id"
	accessKeySecretKey        = "aws_secret_access_key"
	internalELBLabel          = "kubernetes.io/role/internal-elb"
	workName                  = "aws-submariner-gateway-machineset"
	manifestFile              = "pkg/cloud/aws/manifests/machineset.yaml"
	aggeragateClusterroleFile = "pkg/cloud/aws/manifests/machineset-aggeragate-clusterrole.yaml"
)

type machineSetConfig struct {
	InfraId           string
	AZ                string
	AMIId             string
	InstanceType      string
	Region            string
	SecurityGroupName string
	SubnetName        string
}

type awsProvider struct {
	workClinet    workclient.Interface
	awsClinet     client.Interface
	eventRecorder events.Recorder
	region        string
	infraId       string
	ikePort       int64
	nattPort      int64
	clusterName   string
}

func NewAWSProvider(
	kubeClient kubernetes.Interface, workClient workclient.Interface,
	eventRecorder events.Recorder,
	region, infraId, clusterName, credentialsSecretName string,
	ikePort, nattPort int) (*awsProvider, error) {
	if region == "" {
		return nil, fmt.Errorf("cluster region is empty")
	}
	if infraId == "" {
		return nil, fmt.Errorf("cluster infraId is empty")
	}

	if ikePort == 0 {
		ikePort = helpers.SubmarinerIKEPort
	}

	if nattPort == 0 {
		nattPort = helpers.SubmarinerNatTPort
	}

	awsClient, err := client.NewClient(kubeClient, clusterName, credentialsSecretName, region)
	if err != nil {
		return nil, err
	}

	return &awsProvider{
		workClinet:    workClient,
		awsClinet:     awsClient,
		eventRecorder: eventRecorder,
		region:        region,
		infraId:       infraId,
		ikePort:       int64(ikePort),
		nattPort:      int64(nattPort),
		clusterName:   clusterName,
	}, nil
}

func (a *awsProvider) PrepareSubmarinerClusterEnv() error {
	vpc, err := a.findVPC()
	if err != nil {
		return fmt.Errorf("failed to find aws vpc with %s: %v \n", a.infraId, err)
	}

	amiId, err := a.findAMIId(*vpc.VpcId)
	if err != nil {
		return fmt.Errorf("failed to find instance ami with infraID %s and vpcID %s: %v \n", a.infraId, *vpc.VpcId, err)
	}

	// Open submariner route port (4800/UDP) between all master and worker nodes
	if err := a.openRoutePort(vpc); err != nil {
		return fmt.Errorf("failed to update security group with infraID %s and vpcID %s: %v \n", a.infraId, *vpc.VpcId, err)
	}
	a.eventRecorder.Eventf("SubmarinerRoutePortOpened", "the submariner route port is opened on aws")

	// Open IPsec ports (by default, 4500/UDP and 500/UDP) for submariner gateway instances
	if err := a.openIPsecPorts(vpc); err != nil {
		return fmt.Errorf("failed to create security group with infraID %s and vpcID %s: %v \n", a.infraId, *vpc.VpcId, err)
	}
	a.eventRecorder.Eventf("SubmarinerIPsecPortsOpened", "the submariner IPsec ports are opened on aws")

	// Tag one subnet with label kubernetes.io/role/internal-elb for automatic subnet discovery by aws load balancers or
	// ingress controllers
	subnet, err := a.tagSubnet(vpc)
	if err != nil {
		return fmt.Errorf("failed to tag subnet with infraID %s and vpcID %s: %v \n", a.infraId, *vpc.VpcId, err)
	}
	a.eventRecorder.Eventf("SubmarinerSubnetTagged", "the subnet %s is tagged on aws", *subnet.SubnetId)

	// Apply a manifest work to create a MachineSet on managed cluster to create a new aws instance for submariner gateway
	if err := a.deployGatewayNode(*subnet.AvailabilityZone, amiId); err != nil {
		return fmt.Errorf("failed to create MachineSet for %s: %v \n", a.infraId, err)
	}
	a.eventRecorder.Eventf("SubmarinerGatewayManifestworkCreated", "the submariner gateway manifestwork %s is created", workName)

	return nil
}

func (a *awsProvider) CleanUpSubmarinerClusterEnv() error {
	var errs []error
	if err := a.deleteGatewayNode(); err != nil {
		errs = append(errs, fmt.Errorf("failed to delete gateway node for %s: %v \n", a.infraId, err))
	} else {
		a.eventRecorder.Eventf("SubmarinerGatewayManifestworkDeleted", "the submariner gateway manifestwork %s is deleted", workName)
	}

	vpc, err := a.findVPC()
	// cannot find the vpc, the below tasks will not continue, return directly
	if err != nil {
		errs = append(errs, fmt.Errorf("failed to find aws vpc with %s: %v \n", a.infraId, err))
		return operatorhelpers.NewMultiLineAggregate(errs)
	}

	if err := a.untagSubnet(vpc); err != nil {
		errs = append(errs, fmt.Errorf("failed to untag subnet for %s: %v \n", a.infraId, err))
	} else {
		a.eventRecorder.Eventf("SubmarinerSubnetUnTagged", "the subnet is untagged on aws")
	}

	// the ipsec sg may has references, result in the sg cannot be deleted, so we revoke ports here
	if err := a.revokeIPsecPorts(vpc); err != nil {
		errs = append(errs, fmt.Errorf("failed to revoke ipsec ports for %s: %v \n", a.infraId, err))
	} else {
		a.eventRecorder.Eventf("SubmarinerIPsecPortsClosed", "the submariner IPsec ports are closed on aws")
	}

	if err := a.revokeRoutePort(vpc); err != nil {
		errs = append(errs, fmt.Errorf("failed to revoke route port for %s: %v \n", a.infraId, err))
	} else {
		a.eventRecorder.Eventf("SubmarinerRoutePortClosed", "the submariner route port is closed on aws")
	}

	return operatorhelpers.NewMultiLineAggregate(errs)
}

func (a *awsProvider) findVPC() (*ec2.Vpc, error) {
	vpcsOutput, err := a.awsClinet.DescribeVpcs(&ec2.DescribeVpcsInput{
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("tag:Name"),
				Values: []*string{aws.String(fmt.Sprintf("%s-vpc", a.infraId))},
			},
			{
				Name:   aws.String(fmt.Sprintf("tag:kubernetes.io/cluster/%s", a.infraId)),
				Values: []*string{aws.String("owned")},
			},
		},
	})
	if err != nil {
		return nil, err
	}
	if len(vpcsOutput.Vpcs) == 0 {
		return nil, &errors.StatusError{
			ErrStatus: metav1.Status{
				Reason:  metav1.StatusReasonNotFound,
				Message: "there are no vpcs",
			},
		}
	}
	return vpcsOutput.Vpcs[0], nil
}

func (a *awsProvider) findAMIId(vpcId string) (string, error) {
	instancesOutput, err := a.awsClinet.DescribeInstances(&ec2.DescribeInstancesInput{
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("vpc-id"),
				Values: []*string{aws.String(vpcId)},
			},
			{
				Name:   aws.String("tag:Name"),
				Values: []*string{aws.String(fmt.Sprintf("%s-worker*", a.infraId))},
			},
			{
				Name:   aws.String(fmt.Sprintf("tag:kubernetes.io/cluster/%s", a.infraId)),
				Values: []*string{aws.String("owned")},
			},
		},
	})
	if err != nil {
		return "", err
	}
	if len(instancesOutput.Reservations) == 0 {
		return "", &errors.StatusError{
			ErrStatus: metav1.Status{
				Reason:  metav1.StatusReasonNotFound,
				Message: "there are no reservations",
			},
		}
	}
	if len(instancesOutput.Reservations[0].Instances) == 0 {
		return "", &errors.StatusError{
			ErrStatus: metav1.Status{
				Reason:  metav1.StatusReasonNotFound,
				Message: "there are no instances",
			},
		}
	}
	return *instancesOutput.Reservations[0].Instances[0].ImageId, nil
}

func (a *awsProvider) findSecurityGroup(vpcId, nameTag string) (*ec2.SecurityGroup, error) {
	securityGroups, err := a.awsClinet.DescribeSecurityGroups(&ec2.DescribeSecurityGroupsInput{
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("vpc-id"),
				Values: []*string{aws.String(vpcId)},
			},
			{
				Name:   aws.String("tag:Name"),
				Values: []*string{aws.String(nameTag)},
			},
		},
	})
	if err != nil {
		return nil, err
	}
	if len(securityGroups.SecurityGroups) == 0 {
		return nil, &errors.StatusError{
			ErrStatus: metav1.Status{
				Reason:  metav1.StatusReasonNotFound,
				Message: "there are no security groups",
			},
		}
	}
	return securityGroups.SecurityGroups[0], nil
}

func (a *awsProvider) findSubnet(vpcId string) (*ec2.Subnet, error) {
	subnets, err := a.awsClinet.DescribeSubnets(&ec2.DescribeSubnetsInput{
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("vpc-id"),
				Values: []*string{aws.String(vpcId)},
			},
			{
				Name:   aws.String("tag:Name"),
				Values: []*string{aws.String(fmt.Sprintf("%s-public-%s*", a.infraId, a.region))},
			},
			{
				Name:   aws.String(fmt.Sprintf("tag:kubernetes.io/cluster/%s", a.infraId)),
				Values: []*string{aws.String("owned")},
			},
		},
	})
	if err != nil {
		return nil, err
	}
	if len(subnets.Subnets) == 0 {
		return nil, &errors.StatusError{
			ErrStatus: metav1.Status{
				Reason:  metav1.StatusReasonNotFound,
				Message: "there are no subnets",
			},
		}
	}
	return subnets.Subnets[0], nil
}

func (a *awsProvider) openRoutePort(vpc *ec2.Vpc) error {
	workerSecurityGroup, err := a.findSecurityGroup(*vpc.VpcId, fmt.Sprintf("%s-worker-sg", a.infraId))
	if err != nil {
		return err
	}

	masterSecurityGroup, err := a.findSecurityGroup(*vpc.VpcId, fmt.Sprintf("%s-master-sg", a.infraId))
	if err != nil {
		return err
	}

	workerPermission, masterPermission := getRoutePortPermission(masterSecurityGroup, workerSecurityGroup)
	_, err = a.awsClinet.AuthorizeSecurityGroupIngress(&ec2.AuthorizeSecurityGroupIngressInput{
		GroupId:       workerSecurityGroup.GroupId,
		IpPermissions: []*ec2.IpPermission{workerPermission},
	})
	switch {
	case isAWSDuplicatedError(err):
		klog.V(4).Infof("the route port has been opened in security group %s on aws ", *workerSecurityGroup.GroupId)
	case err != nil:
		return err
	}

	_, err = a.awsClinet.AuthorizeSecurityGroupIngress(&ec2.AuthorizeSecurityGroupIngressInput{
		GroupId:       masterSecurityGroup.GroupId,
		IpPermissions: []*ec2.IpPermission{masterPermission},
	})
	switch {
	case isAWSDuplicatedError(err):
		klog.V(4).Infof("the route port has been opened in security group %s on aws", *masterSecurityGroup.GroupId)
	case err != nil:
		return err
	}

	return nil
}

func (a *awsProvider) revokeRoutePort(vpc *ec2.Vpc) error {
	workerSecurityGroup, err := a.findSecurityGroup(*vpc.VpcId, fmt.Sprintf("%s-worker-sg", a.infraId))
	if err != nil {
		return err
	}

	masterSecurityGroup, err := a.findSecurityGroup(*vpc.VpcId, fmt.Sprintf("%s-master-sg", a.infraId))
	if err != nil {
		return err
	}

	workerPermission, masterPermission := getRoutePortPermission(masterSecurityGroup, workerSecurityGroup)

	_, err = a.awsClinet.RevokeSecurityGroupIngress(&ec2.RevokeSecurityGroupIngressInput{
		GroupId:       workerSecurityGroup.GroupId,
		IpPermissions: []*ec2.IpPermission{workerPermission},
	})
	switch {
	case isAWSNotFoundError(err):
		klog.V(4).Infof("there is no route port in security group %s on aws", *workerSecurityGroup.GroupId)
		return nil
	case err != nil:
		return err
	}

	_, err = a.awsClinet.RevokeSecurityGroupIngress(&ec2.RevokeSecurityGroupIngressInput{
		GroupId:       masterSecurityGroup.GroupId,
		IpPermissions: []*ec2.IpPermission{masterPermission},
	})
	switch {
	case isAWSNotFoundError(err):
		klog.V(4).Infof("there is no route port in security group %s on aws", *workerSecurityGroup.GroupId)
		return nil
	case err != nil:
		return err
	}

	return nil
}

func (a *awsProvider) openIPsecPorts(vpc *ec2.Vpc) error {
	permissions := getIPsecPortsPermission(a.ikePort, a.nattPort)
	groupName := fmt.Sprintf("%s-submariner-gw-sg", a.infraId)
	sg, err := a.findSecurityGroup(*vpc.VpcId, groupName)
	if errors.IsNotFound(err) {
		return a.createGatewaySecurityGroup(vpc, groupName, permissions)
	}
	if err != nil {
		return err
	}

	// the rules has been built
	if hasIPsecPorts(sg.IpPermissions, a.ikePort, a.nattPort) {
		klog.V(4).Infof("the IPsec ports has been opened in security group %s on aws", *sg.GroupId)
		return nil
	}

	if len(sg.IpPermissions) != 0 {
		// revoke the old rules
		if _, err = a.awsClinet.RevokeSecurityGroupIngress(&ec2.RevokeSecurityGroupIngressInput{
			GroupId:       sg.GroupId,
			IpPermissions: sg.IpPermissions,
		}); err != nil {
			return err
		}
	}

	_, err = a.awsClinet.AuthorizeSecurityGroupIngress(&ec2.AuthorizeSecurityGroupIngressInput{
		GroupId:       sg.GroupId,
		IpPermissions: permissions,
	})
	return err
}

func (a *awsProvider) createGatewaySecurityGroup(vpc *ec2.Vpc, groupName string, permissions []*ec2.IpPermission) error {
	sg, err := a.awsClinet.CreateSecurityGroup(&ec2.CreateSecurityGroupInput{
		GroupName:   aws.String(groupName),
		VpcId:       vpc.VpcId,
		Description: aws.String("For submariner gateway"),
		TagSpecifications: []*ec2.TagSpecification{
			{
				ResourceType: aws.String("security-group"),
				Tags: []*ec2.Tag{
					{
						Key:   aws.String("Name"),
						Value: aws.String(groupName),
					},
				},
			},
		},
	})
	if err != nil {
		return err
	}
	if _, err := a.awsClinet.AuthorizeSecurityGroupIngress(&ec2.AuthorizeSecurityGroupIngressInput{
		GroupId:       sg.GroupId,
		IpPermissions: permissions,
	}); err != nil {
		return err
	}
	return nil
}

func (a *awsProvider) revokeIPsecPorts(vpc *ec2.Vpc) error {
	groupName := fmt.Sprintf("%s-submariner-gw-sg", a.infraId)
	sg, err := a.findSecurityGroup(*vpc.VpcId, groupName)
	if errors.IsNotFound(err) {
		klog.V(4).Infof("there is no security group %s on aws", groupName)
		return nil
	}
	if err != nil {
		return err
	}

	_, err = a.awsClinet.RevokeSecurityGroupIngress(&ec2.RevokeSecurityGroupIngressInput{
		GroupId:       sg.GroupId,
		IpPermissions: sg.IpPermissions,
	})
	if isAWSNotFoundError(err) {
		klog.V(4).Infof("there is no ipsec ports in security group %s on aws", *sg.GroupId)
		return nil
	}
	return err
}

func (a *awsProvider) tagSubnet(vpc *ec2.Vpc) (*ec2.Subnet, error) {
	subnet, err := a.findSubnet(*vpc.VpcId)
	if err != nil {
		return nil, err
	}

	// the tag has been labeled
	for _, tag := range subnet.Tags {
		if *tag.Key == internalELBLabel {
			klog.V(4).Infof("subnet %s has been tagged with internal ELB label on aws", *subnet.SubnetId)
			return subnet, nil
		}
	}

	if _, err := a.awsClinet.CreateTags(&ec2.CreateTagsInput{
		Tags: []*ec2.Tag{
			{
				Key:   aws.String(internalELBLabel),
				Value: aws.String(""),
			},
		},
		Resources: []*string{
			subnet.SubnetId,
		},
	}); err != nil {
		return nil, err
	}

	return subnet, nil
}

func (a *awsProvider) untagSubnet(vpc *ec2.Vpc) error {
	subnet, err := a.findSubnet(*vpc.VpcId)
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}

	_, err = a.awsClinet.DeleteTags(&ec2.DeleteTagsInput{
		Tags: []*ec2.Tag{
			{
				Key:   aws.String(internalELBLabel),
				Value: aws.String(""),
			},
		},
		Resources: []*string{
			subnet.SubnetId,
		},
	})
	return err
}

func (a *awsProvider) deployGatewayNode(az, amiId string) error {
	clusterRoleYamlData := assets.MustCreateAssetFromTemplate(
		aggeragateClusterroleFile,
		bindata.MustAsset(filepath.Join("", aggeragateClusterroleFile)),
		nil).Data
	clusterRoleJsonData, err := yaml.YAMLToJSON(clusterRoleYamlData)
	if err != nil {
		return err
	}
	msYamlData := assets.MustCreateAssetFromTemplate(
		manifestFile,
		bindata.MustAsset(filepath.Join("", manifestFile)),
		&machineSetConfig{
			InfraId:           a.infraId,
			AZ:                az,
			AMIId:             amiId,
			Region:            a.region,
			SecurityGroupName: fmt.Sprintf("%s-submariner-gw-sg", a.infraId),
			InstanceType:      instanceType,
			SubnetName:        fmt.Sprintf("%s-public-%s", a.infraId, az),
		}).Data
	msJsonData, err := yaml.YAMLToJSON(msYamlData)
	if err != nil {
		return err
	}
	return helpers.ApplyManifestWork(context.TODO(), a.workClinet, &workv1.ManifestWork{
		TypeMeta: metav1.TypeMeta{},
		ObjectMeta: metav1.ObjectMeta{
			Name:      workName,
			Namespace: a.clusterName,
		},
		Spec: workv1.ManifestWorkSpec{
			Workload: workv1.ManifestsTemplate{
				Manifests: []workv1.Manifest{
					{
						RawExtension: runtime.RawExtension{Raw: clusterRoleJsonData},
					},
					{
						RawExtension: runtime.RawExtension{Raw: msJsonData},
					},
				},
			},
		},
	})
}

func (a *awsProvider) deleteGatewayNode() error {
	err := a.workClinet.WorkV1().ManifestWorks(a.clusterName).Delete(context.TODO(), workName, metav1.DeleteOptions{})
	if errors.IsNotFound(err) {
		return nil
	}
	return err
}

func getRoutePortPermission(masterSecurityGroup, workerSecurityGroup *ec2.SecurityGroup) (workerPermission, masterPermission *ec2.IpPermission) {
	return (&ec2.IpPermission{}).
			SetFromPort(helpers.SubmarinerRoutePort).
			SetToPort(helpers.SubmarinerRoutePort).
			SetIpProtocol("udp").
			SetUserIdGroupPairs([]*ec2.UserIdGroupPair{
				{
					// route traffic for all workers
					GroupId: workerSecurityGroup.GroupId,
					UserId:  workerSecurityGroup.OwnerId,
				},
				{
					// route traffic from master nodes to worker nodes
					GroupId: masterSecurityGroup.GroupId,
					UserId:  masterSecurityGroup.OwnerId,
				},
			}), (&ec2.IpPermission{}).
			SetFromPort(helpers.SubmarinerRoutePort).
			SetToPort(helpers.SubmarinerRoutePort).
			SetIpProtocol("udp").
			SetUserIdGroupPairs([]*ec2.UserIdGroupPair{
				{
					// route traffic from worker nodes to master nodes
					GroupId: workerSecurityGroup.GroupId,
					UserId:  workerSecurityGroup.OwnerId,
				},
			})
}

func getIPsecPortsPermission(ikePort, nattPort int64) []*ec2.IpPermission {
	return []*ec2.IpPermission{
		(&ec2.IpPermission{}).
			SetIpProtocol("udp").
			SetFromPort(ikePort).
			SetToPort(ikePort).
			SetIpRanges([]*ec2.IpRange{
				(&ec2.IpRange{}).SetCidrIp("0.0.0.0/0"),
			}),
		(&ec2.IpPermission{}).
			SetIpProtocol("udp").
			SetFromPort(nattPort).
			SetToPort(nattPort).
			SetIpRanges([]*ec2.IpRange{
				(&ec2.IpRange{}).SetCidrIp("0.0.0.0/0"),
			}),
	}
}

func hasIPsecPorts(permissions []*ec2.IpPermission, expectedIKEPort, expectedNatTPort int64) bool {
	if len(permissions) != 2 {
		return false
	}
	ports := make(map[int64]bool)
	ports[*permissions[0].FromPort] = true
	ports[*permissions[1].FromPort] = true
	if _, ok := ports[expectedIKEPort]; !ok {
		return false
	}
	if _, ok := ports[expectedNatTPort]; !ok {
		return false
	}
	return true
}

func isAWSDuplicatedError(err error) bool {
	if awsErr, ok := err.(awserr.Error); ok {
		// we had to hardcoded, see https://github.com/aws/aws-sdk-go/issues/3235
		if awsErr.Code() == "InvalidPermission.Duplicate" {
			return true
		}
	}
	return false
}

func isAWSNotFoundError(err error) bool {
	if awsErr, ok := err.(awserr.Error); ok {
		// we had to hardcoded, see https://github.com/aws/aws-sdk-go/issues/3235
		if awsErr.Code() == "InvalidPermission.NotFound" {
			return true
		}
	}
	return false
}