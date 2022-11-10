package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"cloud.google.com/go/compute/metadata"

	cp "github.com/kubecost/cluster-turndown/v2/pkg/cluster/provider"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/client-go/kubernetes"

	"github.com/rs/zerolog/log"
)

const (
	TurndownNodeLabel         = "cluster-turndown-node"
	TurndownNodeLabelSelector = "cluster-turndown-node=true"
)

// TurndownProvider contains methods used to manage turndown
type TurndownProvider interface {
	IsTurndownNodePool() bool
	CreateSingletonNodePool(labels map[string]string) error
	GetNodePools() ([]cp.NodePool, error)
	GetPoolID(node *v1.Node) string
	SetNodePoolSizes(nodePools []cp.NodePool, size int32) error
	ResetNodePoolSizes(nodePools []cp.NodePool) error
}

// Creates a new TurndownProvider implementation using the kubernetes client instance a ClusterProvider
func NewTurndownProvider(client kubernetes.Interface, clusterProvider cp.ClusterProvider) (TurndownProvider, error) {
	if client == nil {
		return nil, fmt.Errorf("Could not create new TurndownProvider with nil Kubernetes client")
	}
	if clusterProvider == nil {
		return nil, fmt.Errorf("Could not create new TurndownProvider with nil ClusterProvider implementation")
	}

	nodes, err := client.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	if len(nodes.Items) == 0 {
		return nil, fmt.Errorf("Could not locate any Nodes in Kubernetes cluster.")
	}

	if metadata.OnGCE() {
		return NewGKEProvider(client, clusterProvider), nil
	}

	node := nodes.Items[0]
	provider := strings.ToLower(node.Spec.ProviderID)
	if strings.HasPrefix(provider, "aws") {
		if _, ok := node.Labels["eks.amazonaws.com/nodegroup"]; ok {
			log.Info().Msg("Found ProviderID starting with \"aws\" and eks nodegroup, using EKS Provider")
			return NewEKSProvider(client, clusterProvider), nil
		}
		log.Info().Msg("Found ProviderID starting with \"aws\", using AWS Provider")
		return NewAWSProvider(client, clusterProvider), nil
	} else if strings.HasPrefix(provider, "azure") {
		log.Info().Msg("Found ProviderID starting with \"azure\", using Azure Provider")
		return nil, errors.New("Azure Not Supported")
	} else {
		log.Info().Msg("Unsupported provider, falling back to default")
		return nil, errors.New("Custom Not Supported")
	}
}

// Utility function which creates a new map[string]string containing turndown labels in addition
// to the provided labels
func toTurndownNodePoolLabels(labels map[string]string) map[string]string {
	m := map[string]string{
		TurndownNodeLabel: "true",
	}

	for k, v := range labels {
		m[k] = v
	}

	return m
}
