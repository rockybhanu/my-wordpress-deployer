package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"log"
	"path/filepath"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	resource "k8s.io/apimachinery/pkg/api/resource"
	metaV1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// InitKubeClient creates a new Kubernetes clientset using the provided kubeconfig path.
// If kubeconfig is empty, it uses the in-cluster config or the default (~/.kube/config).
func InitKubeClient(kubeconfig string) (*kubernetes.Clientset, error) {
	var config *rest.Config
	var err error

	if kubeconfig != "" {
		// Use the given kubeconfig file
		kubeconfig, err = filepath.Abs(kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("invalid kubeconfig path: %w", err)
		}
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("cannot build config from flags: %w", err)
		}
	} else {
		// Try in-cluster config, fallback to local kube config
		config, err = rest.InClusterConfig()
		if err != nil {
			log.Printf("[WARN] Could not use in-cluster config: %v", err)
			kubeconfigDefault := filepath.Join(HomeDir(), ".kube", "config")
			config, err = clientcmd.BuildConfigFromFlags("", kubeconfigDefault)
			if err != nil {
				return nil, fmt.Errorf("cannot build config from fallback: %w", err)
			}
		}
	}

	clientSet, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create clientset: %w", err)
	}
	return clientSet, nil
}

// HomeDir returns the home directory for the current user (fallback to /root if not set).
func HomeDir() string {
	if h := SystemGetenv("HOME"); h != "" {
		return h
	}
	return "/root"
}

// SystemGetenv can be overridden for testing or special environments.
var SystemGetenv = func(key string) string {
	return ""
}

// ensureNamespace checks if a namespace exists; if not, creates it.
func ensureNamespace(ctx context.Context, clientSet *kubernetes.Clientset, namespace string) error {
	_, err := clientSet.CoreV1().Namespaces().Get(ctx, namespace, metaV1.GetOptions{})
	if err == nil {
		// namespace already exists
		return nil
	}

	nsSpec := &corev1.Namespace{
		ObjectMeta: metaV1.ObjectMeta{
			Name: namespace,
		},
	}

	_, err = clientSet.CoreV1().Namespaces().Create(ctx, nsSpec, metaV1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("unable to create namespace %s: %w", namespace, err)
	}
	return nil
}

// createPersistentVolume creates a hostPath PV with the given capacity (in GB),
// ensuring the directory is created if it doesn't exist.
func createPersistentVolume(ctx context.Context, clientSet *kubernetes.Clientset,
	namespace, pvName, hostPath string, sizeGB int) error {

	quantity, err := resource.ParseQuantity(fmt.Sprintf("%dGi", sizeGB))
	if err != nil {
		return fmt.Errorf("invalid capacity: %w", err)
	}

	hostPathType := corev1.HostPathDirectoryOrCreate

	pv := &corev1.PersistentVolume{
		ObjectMeta: metaV1.ObjectMeta{
			Name: pvName,
			Labels: map[string]string{
				"app": pvName,
			},
		},
		Spec: corev1.PersistentVolumeSpec{
			AccessModes:                   []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			Capacity: corev1.ResourceList{
				corev1.ResourceStorage: quantity,
			},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: hostPath,
					Type: &hostPathType,
				},
			},
		},
	}

	_, err = clientSet.CoreV1().PersistentVolumes().Create(ctx, pv, metaV1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("unable to create PV %s: %w", pvName, err)
	}

	return nil
}

// createPersistentVolumeClaim creates a PVC that references the specified PV (by label selector).
func createPersistentVolumeClaim(ctx context.Context, clientSet *kubernetes.Clientset,
	namespace, pvcName, pvName string, sizeGB int) error {

	quantity, err := resource.ParseQuantity(fmt.Sprintf("%dGi", sizeGB))
	if err != nil {
		return fmt.Errorf("invalid capacity: %w", err)
	}

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metaV1.ObjectMeta{
			Name:      pvcName,
			Namespace: namespace,
			Labels: map[string]string{
				"app": pvcName,
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{
				corev1.ReadWriteOnce,
			},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: quantity,
				},
			},
			Selector: &metaV1.LabelSelector{
				MatchLabels: map[string]string{
					"app": pvName, // match the label "app" on the PV
				},
			},
		},
	}

	_, err = clientSet.CoreV1().PersistentVolumeClaims(namespace).Create(ctx, pvc, metaV1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("unable to create PVC %s: %w", pvcName, err)
	}

	return nil
}

// createWPMySQLSecret generates random passwords and stores all needed environment variables
// for both MySQL and WordPress in a single Secret.
func createWPMySQLSecret(ctx context.Context, clientSet *kubernetes.Clientset, namespace, secretName, deployName string) error {
	// Generate random root password and user password
	rootPass, err := generateRandomPassword(16)
	if err != nil {
		return fmt.Errorf("failed to generate root password: %w", err)
	}
	wpPass, err := generateRandomPassword(16)
	if err != nil {
		return fmt.Errorf("failed to generate wordpress user password: %w", err)
	}

	// We'll define environment variables so MySQL initializes automatically:
	// MYSQL_ROOT_PASSWORD, MYSQL_DATABASE, MYSQL_USER, MYSQL_PASSWORD
	// And WordPress environment variables:
	// WORDPRESS_DB_HOST, WORDPRESS_DB_USER, WORDPRESS_DB_PASSWORD, WORDPRESS_DB_NAME
	// We store them all in one secret as Key=Value pairs.
	secretData := map[string][]byte{
		"MYSQL_ROOT_PASSWORD": []byte(rootPass),
		"MYSQL_DATABASE":      []byte("wordpressdb"),
		"MYSQL_USER":          []byte("wordpress"),
		"MYSQL_PASSWORD":      []byte(wpPass),

		"WORDPRESS_DB_HOST":     []byte(deployName + "-db-svc"), // e.g. "ramanuj-db-svc"
		"WORDPRESS_DB_USER":     []byte("wordpress"),
		"WORDPRESS_DB_PASSWORD": []byte(wpPass),
		"WORDPRESS_DB_NAME":     []byte("wordpressdb"),
	}

	secret := &corev1.Secret{
		ObjectMeta: metaV1.ObjectMeta{
			Name:      secretName,
			Namespace: namespace,
		},
		Type: corev1.SecretTypeOpaque,
		Data: secretData,
	}

	_, err = clientSet.CoreV1().Secrets(namespace).Create(ctx, secret, metaV1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("unable to create secret %s: %w", secretName, err)
	}
	return nil
}

// createMySQLDeployment creates a Deployment for MySQL, mounting the given PVC,
// using environment variables from the combined secret (root password, DB, user, pass).
func createMySQLDeployment(ctx context.Context, clientSet *kubernetes.Clientset,
	namespace, deployName, pvcName, secretName string) error {

	envFromSource := corev1.EnvFromSource{
		SecretRef: &corev1.SecretEnvSource{
			LocalObjectReference: corev1.LocalObjectReference{
				Name: secretName,
			},
		},
	}

	deployment := &appsv1.Deployment{
		ObjectMeta: metaV1.ObjectMeta{
			Name:      deployName,
			Namespace: namespace,
			Labels: map[string]string{
				"app": deployName,
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
			Selector: &metaV1.LabelSelector{
				MatchLabels: map[string]string{
					"app": deployName,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metaV1.ObjectMeta{
					Labels: map[string]string{
						"app": deployName,
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "mysql",
							Image: "mysql:8",
							Ports: []corev1.ContainerPort{
								{
									ContainerPort: 3306,
									Name:          "mysql",
								},
							},
							EnvFrom: []corev1.EnvFromSource{
								envFromSource,
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "mysql-persistent-storage",
									MountPath: "/var/lib/mysql",
								},
							},
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									TCPSocket: &corev1.TCPSocketAction{
										Port: intstr.FromInt(3306),
									},
								},
								InitialDelaySeconds: 10,
								PeriodSeconds:       5,
							},
							LivenessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									TCPSocket: &corev1.TCPSocketAction{
										Port: intstr.FromInt(3306),
									},
								},
								InitialDelaySeconds: 30,
								PeriodSeconds:       10,
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "mysql-persistent-storage",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: pvcName,
								},
							},
						},
					},
				},
			},
		},
	}

	_, err := clientSet.AppsV1().Deployments(namespace).Create(ctx, deployment, metaV1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("unable to create MySQL deployment %s: %w", deployName, err)
	}
	return nil
}

// createMySQLService creates a ClusterIP service for MySQL so WordPress can connect.
func createMySQLService(ctx context.Context, clientSet *kubernetes.Clientset,
	namespace, svcName, deployName string) error {

	service := &corev1.Service{
		ObjectMeta: metaV1.ObjectMeta{
			Name:      svcName,
			Namespace: namespace,
			Labels: map[string]string{
				"app": deployName,
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				"app": deployName,
			},
			Ports: []corev1.ServicePort{
				{
					Name:     "mysql",
					Protocol: corev1.ProtocolTCP,
					Port:     3306,
				},
			},
			Type: corev1.ServiceTypeClusterIP,
		},
	}

	_, err := clientSet.CoreV1().Services(namespace).Create(ctx, service, metaV1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("unable to create MySQL service %s: %w", svcName, err)
	}
	return nil
}

// createWordPressDeployment creates a Deployment for WordPress, mounting the given PVC,
// also using environment variables from the same secret.
func createWordPressDeployment(ctx context.Context, clientSet *kubernetes.Clientset,
	namespace, deployName, pvcName, secretName, dbSvcName string) error {

	// Use EnvFrom to load all WORDPRESS_DB_* environment variables from the secret
	envFromSource := corev1.EnvFromSource{
		SecretRef: &corev1.SecretEnvSource{
			LocalObjectReference: corev1.LocalObjectReference{
				Name: secretName,
			},
		},
	}

	deployment := &appsv1.Deployment{
		ObjectMeta: metaV1.ObjectMeta{
			Name:      deployName,
			Namespace: namespace,
			Labels: map[string]string{
				"app": deployName,
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
			Selector: &metaV1.LabelSelector{
				MatchLabels: map[string]string{
					"app": deployName,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metaV1.ObjectMeta{
					Labels: map[string]string{
						"app": deployName,
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "wordpress",
							Image: "wordpress:6.7.1",
							Ports: []corev1.ContainerPort{
								{
									ContainerPort: 80,
									Name:          "http",
								},
							},
							EnvFrom: []corev1.EnvFromSource{
								envFromSource,
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "wordpress-persistent-storage",
									MountPath: "/var/www/html",
								},
							},
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path: "/",
										Port: intstr.FromInt(80),
									},
								},
								InitialDelaySeconds: 10,
								PeriodSeconds:       5,
							},
							LivenessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path: "/",
										Port: intstr.FromInt(80),
									},
								},
								InitialDelaySeconds: 30,
								PeriodSeconds:       10,
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "wordpress-persistent-storage",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: pvcName,
								},
							},
						},
					},
				},
			},
		},
	}

	_, err := clientSet.AppsV1().Deployments(namespace).Create(ctx, deployment, metaV1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("unable to create WordPress deployment %s: %w", deployName, err)
	}
	return nil
}

// createWordPressService creates a ClusterIP service for WordPress on port 80.
func createWordPressService(ctx context.Context, clientSet *kubernetes.Clientset,
	namespace, svcName, deployName string) error {

	service := &corev1.Service{
		ObjectMeta: metaV1.ObjectMeta{
			Name:      svcName,
			Namespace: namespace,
			Labels: map[string]string{
				"app": deployName,
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				"app": deployName,
			},
			Ports: []corev1.ServicePort{
				{
					Name:     "http",
					Protocol: corev1.ProtocolTCP,
					Port:     80,
				},
			},
			Type: corev1.ServiceTypeClusterIP,
		},
	}

	_, err := clientSet.CoreV1().Services(namespace).Create(ctx, service, metaV1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("unable to create WordPress service %s: %w", svcName, err)
	}
	return nil
}

// waitForDeploymentReady polls the deployment until it has at least one ready replica or times out.
func waitForDeploymentReady(ctx context.Context, clientSet *kubernetes.Clientset,
	namespace, deployName string, timeout time.Duration) error {

	log.Printf("[INFO] Checking readiness for deployment: %s/%s", namespace, deployName)
	return wait.PollImmediate(5*time.Second, timeout, func() (bool, error) {
		deploy, err := clientSet.AppsV1().Deployments(namespace).Get(ctx, deployName, metaV1.GetOptions{})
		if err != nil {
			log.Printf("[WARN] Error fetching deployment status: %v", err)
			// Could be transient, keep retrying
			return false, nil
		}

		if deploy.Status.ReadyReplicas >= 1 {
			return true, nil
		}
		log.Printf("[DEBUG] Deployment %s not ready yet. ReadyReplicas=%d, Replicas=%d",
			deployName, deploy.Status.ReadyReplicas, deploy.Status.Replicas)
		return false, nil
	})
}

// int32Ptr is a simple helper for pointer values.
func int32Ptr(i int32) *int32 {
	return &i
}

// generateRandomPassword returns a random string of the specified length using a secure RNG.
func generateRandomPassword(length int) (string, error) {
	const chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789!@#$%^&*()-_+"
	bytes := make([]byte, length)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	for i := 0; i < length; i++ {
		bytes[i] = chars[bytes[i]%byte(len(chars))]
	}
	return string(bytes), nil
}
