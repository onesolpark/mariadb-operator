package builder

import (
	"errors"
	"fmt"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	labels "github.com/mariadb-operator/mariadb-operator/pkg/builder/labels"
	metadata "github.com/mariadb-operator/mariadb-operator/pkg/builder/metadata"
	"github.com/mariadb-operator/mariadb-operator/pkg/datastructures"
	kadapter "github.com/mariadb-operator/mariadb-operator/pkg/kubernetes/adapter"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	MetricsPortName                = "metrics"
	deployConfigVolume             = "config"
	deployConfigMountPath          = "/etc/config/"
	maxScaleRuntimeConfigVolume    = "runtime-config"
	maxScaleRuntimeConfigMountPath = "/var/lib/maxscale/maxscale.cnf.d"
)

func (b *Builder) BuildExporterDeployment(mariadb *mariadbv1alpha1.MariaDB,
	podAnnotations map[string]string) (*appsv1.Deployment, error) {
	if !mariadb.AreMetricsEnabled() {
		return nil, errors.New("MariaDB instance does not specify Metrics")
	}
	key := mariadb.MetricsKey()
	config := mariadb.MetricsConfigSecretKeyRef()
	objMeta :=
		metadata.NewMetadataBuilder(key).
			WithMetadata(mariadb.Spec.InheritMetadata).
			Build()
	selectorLabels :=
		labels.NewLabelsBuilder().
			WithMetricsSelectorLabels(key).
			Build()
	exporter := ptr.Deref(mariadb.Spec.Metrics, mariadbv1alpha1.MariadbMetrics{}).Exporter
	podObjMeta :=
		metadata.NewMetadataBuilder(key).
			WithMetadata(mariadb.Spec.InheritMetadata).
			WithMetadata(exporter.PodMetadata).
			WithAnnotations(podAnnotations).
			WithLabels(selectorLabels).
			Build()

	podTemplate, err := b.exporterPodTemplate(
		podObjMeta,
		&exporter,
		[]string{
			fmt.Sprintf("--config.my-cnf=%s", exporterConfigFile(config.Key)),
		},
		mariadb.Spec.ImagePullSecrets,
		config.Name,
	)
	if err != nil {
		return nil, fmt.Errorf("error building exporter pod template: %v", err)
	}

	deployment := &appsv1.Deployment{
		ObjectMeta: objMeta,
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: selectorLabels,
			},
			Template: *podTemplate,
		},
	}
	if err := controllerutil.SetControllerReference(mariadb, deployment, b.scheme); err != nil {
		return nil, fmt.Errorf("error setting controller reference to Deployment: %v", err)
	}
	return deployment, nil
}

func (b *Builder) BuildMaxScaleExporterDeployment(mxs *mariadbv1alpha1.MaxScale,
	podAnnotations map[string]string) (*appsv1.Deployment, error) {
	if !mxs.AreMetricsEnabled() {
		return nil, errors.New("MaxScale instance does not specify Metrics")
	}
	key := mxs.MetricsKey()
	config := mxs.MetricsConfigSecretKeyRef()
	objMeta :=
		metadata.NewMetadataBuilder(key).
			WithMetadata(mxs.Spec.InheritMetadata).
			Build()
	selectorLabels :=
		labels.NewLabelsBuilder().
			WithMetricsSelectorLabels(key).
			Build()
	exporter := ptr.Deref(mxs.Spec.Metrics, mariadbv1alpha1.MaxScaleMetrics{}).Exporter
	podObjMeta :=
		metadata.NewMetadataBuilder(key).
			WithMetadata(mxs.Spec.InheritMetadata).
			WithMetadata(exporter.PodMetadata).
			WithAnnotations(podAnnotations).
			WithLabels(selectorLabels).
			Build()

	podTemplate, err := b.exporterPodTemplate(
		podObjMeta,
		&exporter,
		[]string{
			fmt.Sprintf("--config=%s", exporterConfigFile(config.Key)),
		},
		mxs.Spec.ImagePullSecrets,
		config.Name,
	)
	if err != nil {
		return nil, fmt.Errorf("error building MaxScale exporter pod template: %v", err)
	}

	deployment := &appsv1.Deployment{
		ObjectMeta: objMeta,
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: selectorLabels,
			},
			Template: *podTemplate,
		},
	}
	if err := controllerutil.SetControllerReference(mxs, deployment, b.scheme); err != nil {
		return nil, fmt.Errorf("error setting controller reference to Deployment: %v", err)
	}
	return deployment, nil
}

func (b *Builder) exporterPodTemplate(objMeta metav1.ObjectMeta, exporter *mariadbv1alpha1.Exporter, args []string,
	pullSecrets []mariadbv1alpha1.LocalObjectReference, configSecretName string) (*corev1.PodTemplateSpec, error) {
	securityContext, err := b.buildPodSecurityContext(exporter.PodSecurityContext)
	if err != nil {
		return nil, err
	}

	container, err := b.exporterContainer(exporter, args)
	if err != nil {
		return nil, fmt.Errorf("error building exporter container: %v", err)
	}

	affinity := ptr.Deref(exporter.Affinity, mariadbv1alpha1.AffinityConfig{}).Affinity

	return &corev1.PodTemplateSpec{
		ObjectMeta: objMeta,
		Spec: corev1.PodSpec{
			ImagePullSecrets: kadapter.ToKubernetesSlice(datastructures.Merge(pullSecrets, exporter.ImagePullSecrets)),
			Containers: []corev1.Container{
				*container,
			},
			Volumes: []corev1.Volume{
				{
					Name: deployConfigVolume,
					VolumeSource: corev1.VolumeSource{
						Secret: &corev1.SecretVolumeSource{
							SecretName: configSecretName,
						},
					},
				},
			},
			SecurityContext:   securityContext,
			Affinity:          ptr.To(affinity.ToKubernetesType()),
			NodeSelector:      exporter.NodeSelector,
			Tolerations:       exporter.Tolerations,
			PriorityClassName: ptr.Deref(exporter.PriorityClassName, ""),
		},
	}, nil
}

func (b *Builder) exporterContainer(exporter *mariadbv1alpha1.Exporter, args []string) (*corev1.Container, error) {
	securityContext, err := b.buildContainerSecurityContext(exporter.SecurityContext)
	if err != nil {
		return nil, fmt.Errorf("error building container security context: %v", err)
	}

	var resources corev1.ResourceRequirements
	if exporter.Resources != nil {
		resources = exporter.Resources.ToKubernetesType()
	}

	probe := &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: "/",
				Port: intstr.FromInt(int(exporter.Port)),
			},
		},
	}

	return &corev1.Container{
		Name:            "exporter",
		Image:           exporter.Image,
		ImagePullPolicy: exporter.ImagePullPolicy,
		Args:            args,
		Ports: []corev1.ContainerPort{
			{
				Name:          MetricsPortName,
				ContainerPort: exporter.Port,
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      deployConfigVolume,
				MountPath: deployConfigMountPath,
				ReadOnly:  true,
			},
		},
		Resources:       resources,
		SecurityContext: securityContext,
		LivenessProbe:   probe,
		ReadinessProbe:  probe,
	}, nil
}

func exporterConfigFile(fileName string) string {
	return deployConfigMountPath + fileName
}
