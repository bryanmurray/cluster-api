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

package internal

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"time"

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1alpha3"
	"sigs.k8s.io/cluster-api/controllers/remote"
	controlplanev1 "sigs.k8s.io/cluster-api/controlplane/kubeadm/api/v1alpha3"
	"sigs.k8s.io/cluster-api/controlplane/kubeadm/internal/etcd"
	etcdutil "sigs.k8s.io/cluster-api/controlplane/kubeadm/internal/etcd/util"
	"sigs.k8s.io/cluster-api/controlplane/kubeadm/internal/proxy"
	"sigs.k8s.io/cluster-api/util/certs"
	"sigs.k8s.io/cluster-api/util/secret"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

// ManagementCluster holds operations on the ManagementCluster
type ManagementCluster struct {
	Client ctrlclient.Client
}

// OwnedControlPlaneMachines returns a MachineFilter function to find all owned control plane machines.
// Usage: managementCluster.GetMachinesForCluster(ctx, cluster, OwnedControlPlaneMachines(controlPlane.Name))
func OwnedControlPlaneMachines(controlPlaneName string) func(machine *clusterv1.Machine) bool {
	return func(machine *clusterv1.Machine) bool {
		if machine == nil {
			return false
		}
		controllerRef := metav1.GetControllerOf(machine)
		if controllerRef == nil {
			return false
		}
		return controllerRef.Kind == "KubeadmControlPlane" && controllerRef.Name == controlPlaneName
	}
}

// HasDeletionTimestamp returns a MachineFilter function to find all machines
// that have a deletion timestamp.
func HasDeletionTimestamp() func(machine *clusterv1.Machine) bool {
	return func(machine *clusterv1.Machine) bool {
		return machine.GetDeletionTimestamp() != nil
	}
}

// HasOutdatedConfiguration returns a MachineFilter function to find all machines
// that do not match a given KubeadmControlPlane configuration hash.
func HasOutdatedConfiguration(configHash string) func(machine *clusterv1.Machine) bool {
	return func(machine *clusterv1.Machine) bool {
		if machine == nil {
			return false
		}
		return !MatchesConfigurationHash(configHash)(machine)
	}
}

// MatchesConfigurationHash returns a MachineFilter function to find all machines
// that match a given KubeadmControlPlane configuration hash.
func MatchesConfigurationHash(configHash string) func(machine *clusterv1.Machine) bool {
	return func(machine *clusterv1.Machine) bool {
		if machine == nil {
			return false
		}
		if hash, ok := machine.Labels[controlplanev1.KubeadmControlPlaneHashLabelKey]; ok {
			return hash == configHash
		}
		return false
	}
}

// OlderThan returns a MachineFilter function to find all machines
// that have a CreationTimestamp earlier than the given time.
func OlderThan(t *metav1.Time) func(machine *clusterv1.Machine) bool {
	return func(machine *clusterv1.Machine) bool {
		if machine == nil {
			return false
		}
		return machine.CreationTimestamp.Before(t)
	}
}

// FilterMachines returns a filtered list of machines
func FilterMachines(machines []*clusterv1.Machine, filters ...func(machine *clusterv1.Machine) bool) []*clusterv1.Machine {
	if len(filters) == 0 {
		return machines
	}

	filteredMachines := make([]*clusterv1.Machine, 0, len(machines))
	for _, machine := range machines {
		add := true
		for _, filter := range filters {
			if !filter(machine) {
				add = false
				break
			}
		}
		if add {
			filteredMachines = append(filteredMachines, machine)
		}
	}
	return filteredMachines
}

// GetMachinesForCluster returns a list of machines that can be filtered or not.
// If no filter is supplied then all machines associated with the target cluster are returned.
func (m *ManagementCluster) GetMachinesForCluster(ctx context.Context, cluster types.NamespacedName, filters ...func(machine *clusterv1.Machine) bool) ([]*clusterv1.Machine, error) {
	selector := map[string]string{
		clusterv1.ClusterLabelName: cluster.Name,
	}
	ml := &clusterv1.MachineList{}
	if err := m.Client.List(ctx, ml, client.InNamespace(cluster.Namespace), client.MatchingLabels(selector)); err != nil {
		return nil, errors.Wrap(err, "failed to list machines")
	}

	machines := make([]*clusterv1.Machine, 0, len(ml.Items))
	for i := range ml.Items {
		machines = append(machines, &ml.Items[i])
	}

	return FilterMachines(machines, filters...), nil
}

// getCluster builds a cluster object.
// The cluster is also populated with secrets stored on the management cluster that is required for
// secure internal pod connections.
func (m *ManagementCluster) getCluster(ctx context.Context, clusterKey types.NamespacedName) (*cluster, error) {
	// This adapter is for interop with the `remote` package.
	adapterCluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: clusterKey.Namespace,
			Name:      clusterKey.Name,
		},
	}

	// TODO(chuckha): Unroll remote.NewClusterClient if we are unhappy with getting a restConfig twice.
	// TODO(chuckha): Inject this dependency if necessary.
	restConfig, err := remote.RESTConfig(ctx, m.Client, adapterCluster)
	if err != nil {
		return nil, err
	}

	c, err := remote.NewClusterClient(ctx, m.Client, adapterCluster, scheme.Scheme)
	if err != nil {
		return nil, err
	}
	etcdCACert, etcdCAKey, err := m.GetEtcdCerts(ctx, clusterKey)
	if err != nil {
		return nil, err
	}
	return &cluster{
		client:     c,
		restConfig: restConfig,
		etcdCACert: etcdCACert,
		etcdCAkey:  etcdCAKey,
	}, nil
}

// GetEtcdCerts returns the EtcdCA Cert and Key for a given cluster.
func (m *ManagementCluster) GetEtcdCerts(ctx context.Context, cluster types.NamespacedName) ([]byte, []byte, error) {
	etcdCASecret := &corev1.Secret{}
	etcdCAObjectKey := types.NamespacedName{
		Namespace: cluster.Namespace,
		Name:      fmt.Sprintf("%s-etcd", cluster.Name),
	}
	if err := m.Client.Get(ctx, etcdCAObjectKey, etcdCASecret); err != nil {
		return nil, nil, errors.Wrapf(err, "failed to get secret; etcd CA bundle %s/%s", etcdCAObjectKey.Namespace, etcdCAObjectKey.Name)
	}
	crtData, ok := etcdCASecret.Data[secret.TLSCrtDataName]
	if !ok {
		return nil, nil, errors.Errorf("etcd tls crt does not exist for cluster %s/%s", cluster.Namespace, cluster.Name)
	}
	keyData, ok := etcdCASecret.Data[secret.TLSKeyDataName]
	if !ok {
		return nil, nil, errors.Errorf("etcd tls key does not exist for cluster %s/%s", cluster.Namespace, cluster.Name)
	}
	return crtData, keyData, nil
}

type healthCheck func(context.Context) (healthCheckResult, error)

// healthCheck will run a generic health check function and report any errors discovered.
// It does some additional validation to make sure there is a 1;1 match between nodes and machines.
func (m *ManagementCluster) healthCheck(ctx context.Context, check healthCheck, clusterKey types.NamespacedName, controlPlaneName string) error {
	nodeChecks, err := check(ctx)
	if err != nil {
		return err
	}
	errorList := []error{}
	for nodeName, err := range nodeChecks {
		if err != nil {
			errorList = append(errorList, fmt.Errorf("node %q: %v", nodeName, err))
		}
	}
	if len(errorList) != 0 {
		return kerrors.NewAggregate(errorList)
	}

	// Make sure Cluster API is aware of all the nodes.
	machines, err := m.GetMachinesForCluster(ctx, clusterKey, OwnedControlPlaneMachines(controlPlaneName))
	if err != nil {
		return err
	}

	// This check ensures there is a 1 to 1 correspondence of nodes and machines.
	// If a machine was not checked this is considered an error.
	for _, machine := range machines {
		if machine.Status.NodeRef == nil {
			return errors.Errorf("control plane machine %s/%s has no status.nodeRef", machine.Namespace, machine.Name)
		}
		if _, ok := nodeChecks[machine.Status.NodeRef.Name]; !ok {
			return errors.Errorf("machine's (%s/%s) node (%s) was not checked", machine.Namespace, machine.Name, machine.Status.NodeRef.Name)
		}
	}
	if len(nodeChecks) != len(machines) {
		return errors.Errorf("number of nodes and machines in namespace %s did not match: %d nodes %d machines", clusterKey.Namespace, len(nodeChecks), len(machines))
	}
	return nil
}

// TargetClusterControlPlaneIsHealthy checks every node for control plane health.
func (m *ManagementCluster) TargetClusterControlPlaneIsHealthy(ctx context.Context, clusterKey types.NamespacedName, controlPlaneName string) error {
	cluster, err := m.getCluster(ctx, clusterKey)
	if err != nil {
		return err
	}
	return m.healthCheck(ctx, cluster.controlPlaneIsHealthy, clusterKey, controlPlaneName)
}

// TargetClusterEtcdIsHealthy runs a series of checks over a target cluster's etcd cluster.
// In addition, it verifies that there are the same number of etcd members as control plane Machines.
func (m *ManagementCluster) TargetClusterEtcdIsHealthy(ctx context.Context, clusterKey types.NamespacedName, controlPlaneName string) error {
	cluster, err := m.getCluster(ctx, clusterKey)
	if err != nil {
		return err
	}
	return m.healthCheck(ctx, cluster.etcdIsHealthy, clusterKey, controlPlaneName)
}

// cluster are operations on target clusters.
type cluster struct {
	client ctrlclient.Client
	// restConfig is required for the proxy.
	restConfig            *rest.Config
	etcdCACert, etcdCAkey []byte
}

// generateEtcdTLSClientBundle builds an etcd client TLS bundle from the Etcd CA for this cluster.
func (c *cluster) generateEtcdTLSClientBundle() (*tls.Config, error) {
	clientCert, err := generateClientCert(c.etcdCACert, c.etcdCAkey)
	if err != nil {
		return nil, err
	}

	caPool := x509.NewCertPool()
	caPool.AppendCertsFromPEM(c.etcdCACert)

	return &tls.Config{
		RootCAs:      caPool,
		Certificates: []tls.Certificate{clientCert},
	}, nil
}

func (c *cluster) getControlPlaneNodes(ctx context.Context) (*corev1.NodeList, error) {
	nodes := &corev1.NodeList{}
	labels := map[string]string{
		"node-role.kubernetes.io/master": "",
	}

	if err := c.client.List(ctx, nodes, client.MatchingLabels(labels)); err != nil {
		return nil, err
	}
	return nodes, nil
}

// healthCheckResult maps nodes that are checked to any errors the node has related to the check.
type healthCheckResult map[string]error

// controlPlaneIsHealthy does a best effort check of the control plane components the kubeadm control plane cares about.
// The return map is a map of node names as keys to error that that node encountered.
// All nodes will exist in the map with nil errors if there were no errors for that node.
func (c *cluster) controlPlaneIsHealthy(ctx context.Context) (healthCheckResult, error) {
	controlPlaneNodes, err := c.getControlPlaneNodes(ctx)
	if err != nil {
		return nil, err
	}

	response := make(map[string]error)
	for _, node := range controlPlaneNodes.Items {
		name := node.Name
		response[name] = nil
		apiServerPodKey := types.NamespacedName{
			Namespace: metav1.NamespaceSystem,
			Name:      staticPodName("kube-apiserver", name),
		}
		apiServerPod := &corev1.Pod{}
		if err := c.client.Get(ctx, apiServerPodKey, apiServerPod); err != nil {
			response[name] = err
			continue
		}
		response[name] = checkStaticPodReadyCondition(apiServerPod)

		controllerManagerPodKey := types.NamespacedName{
			Namespace: metav1.NamespaceSystem,
			Name:      staticPodName("kube-controller-manager", name),
		}
		controllerManagerPod := &corev1.Pod{}
		if err := c.client.Get(ctx, controllerManagerPodKey, controllerManagerPod); err != nil {
			response[name] = err
			continue
		}
		response[name] = checkStaticPodReadyCondition(controllerManagerPod)
	}

	return response, nil
}

// etcdIsHealthy runs checks for every etcd member in the cluster to satisfy our definition of healthy.
// This is a best effort check and nodes can become unhealthy after the check is complete. It is not a guarantee.
// It's used a signal for if we should allow a target cluster to scale up, scale down or upgrade.
// It returns a map of nodes checked along with an error for a given node.
func (c *cluster) etcdIsHealthy(ctx context.Context) (healthCheckResult, error) {
	var knownClusterID uint64
	var knownMemberIDSet etcdutil.UInt64Set

	controlPlaneNodes, err := c.getControlPlaneNodes(ctx)
	if err != nil {
		return nil, err
	}

	tlsConfig, err := c.generateEtcdTLSClientBundle()
	if err != nil {
		return nil, err
	}

	response := make(map[string]error)
	for _, node := range controlPlaneNodes.Items {
		name := node.Name
		response[name] = nil
		if node.Spec.ProviderID == "" {
			response[name] = errors.New("empty provider ID")
			continue
		}

		// Create the etcd client for the etcd Pod scheduled on the Node
		etcdClient, err := c.getEtcdClientForNode(name, tlsConfig)
		if err != nil {
			response[name] = errors.Wrap(err, "failed to create etcd client")
			continue
		}

		// List etcd members. This checks that the member is healthy, because the request goes through consensus.
		members, err := etcdClient.Members(ctx)
		if err != nil {
			response[name] = errors.Wrap(err, "failed to list etcd members using etcd client")
			continue
		}
		member := etcdutil.MemberForName(members, name)

		// Check that the member reports no alarms.
		if len(member.Alarms) > 0 {
			response[name] = errors.Errorf("etcd member reports alarms: %v", member.Alarms)
			continue
		}

		// Check that the member belongs to the same cluster as all other members.
		clusterID := member.ClusterID
		if knownClusterID == 0 {
			knownClusterID = clusterID
		} else if knownClusterID != clusterID {
			response[name] = errors.Errorf("etcd member has cluster ID %d, but all previously seen etcd members have cluster ID %d", clusterID, knownClusterID)
			continue
		}

		// Check that the member list is stable.
		memberIDSet := etcdutil.MemberIDSet(members)
		if knownMemberIDSet.Len() == 0 {
			knownMemberIDSet = memberIDSet
		} else {
			unknownMembers := memberIDSet.Difference(knownMemberIDSet)
			if unknownMembers.Len() > 0 {
				response[name] = errors.Errorf("etcd member reports members IDs %v, but all previously seen etcd members reported member IDs %v", memberIDSet.UnsortedList(), knownMemberIDSet.UnsortedList())
			}
			continue
		}
	}

	// Check that there is exactly one etcd member for every control plane machine.
	// There should be no etcd members added "out of band.""
	if len(controlPlaneNodes.Items) != len(knownMemberIDSet) {
		return response, errors.Errorf("there are %d control plane nodes, but %d etcd members", len(controlPlaneNodes.Items), len(knownMemberIDSet))
	}

	return response, nil
}

// getEtcdClientForNode returns a client that talks directly to an etcd instance living on a particular node.
func (c *cluster) getEtcdClientForNode(nodeName string, tlsConfig *tls.Config) (*etcd.Client, error) {
	// This does not support external etcd.
	p := proxy.Proxy{
		Kind:         "pods",
		Namespace:    "kube-system", // TODO, can etcd ever run in a different namespace?
		ResourceName: staticPodName("etcd", nodeName),
		KubeConfig:   c.restConfig,
		TLSConfig:    tlsConfig,
		Port:         2379, // TODO: the pod doesn't expose a port. Is this a problem?
	}
	dialer, err := proxy.NewDialer(p)
	if err != nil {
		return nil, err
	}
	etcdclient, err := etcd.NewEtcdClient("127.0.0.1", dialer.DialContextWithAddr, tlsConfig)
	if err != nil {
		return nil, err
	}
	customClient, err := etcd.NewClientWithEtcd(etcdclient)
	if err != nil {
		return nil, err
	}
	return customClient, nil
}

func generateClientCert(caCertEncoded, caKeyEncoded []byte) (tls.Certificate, error) {
	privKey, err := certs.NewPrivateKey()
	if err != nil {
		return tls.Certificate{}, err
	}
	caCert, err := certs.DecodeCertPEM(caCertEncoded)
	if err != nil {
		return tls.Certificate{}, err
	}
	caKey, err := certs.DecodePrivateKeyPEM(caKeyEncoded)
	if err != nil {
		return tls.Certificate{}, err
	}
	x509Cert, err := newClientCert(caCert, privKey, caKey)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.X509KeyPair(certs.EncodeCertPEM(x509Cert), certs.EncodePrivateKeyPEM(privKey))
}

func newClientCert(caCert *x509.Certificate, key *rsa.PrivateKey, caKey *rsa.PrivateKey) (*x509.Certificate, error) {
	cfg := certs.Config{
		CommonName: "cluster-api.x-k8s.io",
	}

	now := time.Now().UTC()

	tmpl := x509.Certificate{
		SerialNumber: new(big.Int).SetInt64(0),
		Subject: pkix.Name{
			CommonName:   cfg.CommonName,
			Organization: cfg.Organization,
		},
		NotBefore:   now.Add(time.Minute * -5),
		NotAfter:    now.Add(time.Hour * 24 * 365 * 10), // 10 years
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}

	b, err := x509.CreateCertificate(rand.Reader, &tmpl, caCert, key.Public(), caKey)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create signed client certificate: %+v", tmpl)
	}

	c, err := x509.ParseCertificate(b)
	return c, errors.WithStack(err)
}

func staticPodName(component, nodeName string) string {
	return fmt.Sprintf("%s-%s", component, nodeName)
}

func checkStaticPodReadyCondition(pod *corev1.Pod) error {
	found := false
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			found = true
		}
		if condition.Type == corev1.PodReady && condition.Status != corev1.ConditionTrue {
			return errors.Errorf("static pod %s/%s is not ready", pod.Namespace, pod.Name)
		}
	}
	if !found {
		return errors.Errorf("pod does not have ready condition: %v", pod.Name)
	}
	return nil
}
