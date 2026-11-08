package accelerator

import (
	"testing"

	"github.com/stretchr/testify/assert"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
	llmdvariant "github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
)

func TestGetAcceleratorNameFromScaleTarget(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		va         *llmdvariant.Variant
		deployment *appsv1.Deployment
		expected   string
	}{
		{
			name: "nvidia_gpu_from_nodeSelector",
			va:   &llmdvariant.Variant{},
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							NodeSelector: map[string]string{
								"nvidia.com/gpu.product": "Tesla-T4",
							},
						},
					},
				},
			},
			expected: "Tesla-T4",
		},
		{
			name: "amd_gpu_from_nodeSelector",
			va:   &llmdvariant.Variant{},
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							NodeSelector: map[string]string{
								"amd.com/gpu.product-name": "MI250",
							},
						},
					},
				},
			},
			expected: "MI250",
		},
		{
			name: "gke_accelerator_from_nodeSelector",
			va:   &llmdvariant.Variant{},
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							NodeSelector: map[string]string{
								"cloud.google.com/gke-accelerator": "nvidia-tesla-v100",
							},
						},
					},
				},
			},
			expected: "nvidia-tesla-v100",
		},
		{
			// The rest of the documented model-name keys, one case each. Each was
			// "unknown" before, so a fleet pinned this way had its GPUs unattributed
			// and could not use the gpu-inventory limiter.
			name: "eks_auto_mode_instance_gpu_name",
			va:   &llmdvariant.Variant{},
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							NodeSelector: map[string]string{
								"eks.amazonaws.com/instance-gpu-name": "h100",
							},
						},
					},
				},
			},
			expected: "h100",
		},
		{
			name: "karpenter_aws_instance_gpu_name",
			va:   &llmdvariant.Variant{},
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							NodeSelector: map[string]string{
								"karpenter.k8s.aws/instance-gpu-name": "a10g",
							},
						},
					},
				},
			},
			expected: "a10g",
		},
		{
			name: "karpenter_azure_sku_gpu_name",
			va:   &llmdvariant.Variant{},
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							NodeSelector: map[string]string{
								"karpenter.azure.com/sku-gpu-name": "A100",
							},
						},
					},
				},
			},
			expected: "A100",
		},
		{
			name: "coreweave_nvidia_class_from_nodeSelector",
			va:   &llmdvariant.Variant{},
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							NodeSelector: map[string]string{
								"gpu.nvidia.com/class": "A100_NVLINK_80GB",
							},
						},
					},
				},
			},
			expected: "A100_NVLINK_80GB",
		},
		{
			name: "amd_standalone_labeller_beta_prefix",
			va:   &llmdvariant.Variant{},
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							NodeSelector: map[string]string{
								"beta.amd.com/gpu.product-name": "AMD_Instinct_MI300X_OAM",
							},
						},
					},
				},
			},
			expected: "AMD_Instinct_MI300X_OAM",
		},
		{
			// Sharing modes mutate the value and we pass it through unchanged. A MIG
			// slice is not a whole A100 and must not share its quota, so keeping them
			// distinct is the point, not an oversight.
			name: "mig_profile_is_its_own_accelerator",
			va:   &llmdvariant.Variant{},
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							NodeSelector: map[string]string{
								"nvidia.com/gpu.product": "A100-SXM4-40GB-MIG-1g.5gb",
							},
						},
					},
				},
			},
			expected: "A100-SXM4-40GB-MIG-1g.5gb",
		},
		{
			// Time-slicing, same reasoning.
			name: "time_sliced_gpu_is_its_own_accelerator",
			va:   &llmdvariant.Variant{},
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							NodeSelector: map[string]string{
								"nvidia.com/gpu.product": "Tesla-T4-SHARED",
							},
						},
					},
				},
			},
			expected: "Tesla-T4-SHARED",
		},
		{
			// A vendor label is NOT a model label. accelerator=nvidia (AKS) would
			// merge an A100 fleet with an H100 one into a single accelerator, which is
			// worse than unknown -- unknown at least reads as "any GPU can serve this".
			name: "vendor_only_label_must_not_resolve",
			va:   &llmdvariant.Variant{},
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							NodeSelector: map[string]string{
								"accelerator":                      "nvidia",
								"k8s.amazonaws.com/accelerator":    "nvidia-tesla-v100",
								"kubernetes.azure.com/accelerator": "nvidia",
							},
						},
					},
				},
			},
			expected: constants.DefaultAcceleratorName,
		},
		{
			// CoreWeave. Reported against ff81a308: every pod ran on an H200 and the
			// controller still called four GPUs unattributed, because this key was
			// not among the ones it looks at.
			name: "coreweave_nvidia_model_from_nodeSelector",
			va:   &llmdvariant.Variant{},
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							NodeSelector: map[string]string{
								"gpu.nvidia.com/model": "H200",
							},
						},
					},
				},
			},
			expected: "H200",
		},
		{
			name: "intel_gaudi_from_nodeSelector",
			va:   &llmdvariant.Variant{},
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							NodeSelector: map[string]string{
								"habana.ai/product.name": "Intel-Gaudi-2-96GB",
							},
						},
					},
				},
			},
			expected: "Intel-Gaudi-2-96GB",
		},
		{
			name: "intel_gpu_from_nodeSelector",
			va:   &llmdvariant.Variant{},
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							NodeSelector: map[string]string{
								"gpu.intel.com/product": "Max_1100",
							},
						},
					},
				},
			},
			expected: "Max_1100",
		},
		{
			name: "nvidia_gpu_from_required_nodeAffinity",
			va:   &llmdvariant.Variant{},
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Affinity: &corev1.Affinity{
								NodeAffinity: &corev1.NodeAffinity{
									RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
										NodeSelectorTerms: []corev1.NodeSelectorTerm{
											{
												MatchExpressions: []corev1.NodeSelectorRequirement{
													{
														Key:      "nvidia.com/gpu.product",
														Operator: corev1.NodeSelectorOpIn,
														Values:   []string{"A100"},
													},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			expected: "A100",
		},
		{
			name: "amd_gpu_from_preferred_nodeAffinity",
			va:   &llmdvariant.Variant{},
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Affinity: &corev1.Affinity{
								NodeAffinity: &corev1.NodeAffinity{
									PreferredDuringSchedulingIgnoredDuringExecution: []corev1.PreferredSchedulingTerm{
										{
											Weight: 1,
											Preference: corev1.NodeSelectorTerm{
												MatchExpressions: []corev1.NodeSelectorRequirement{
													{
														Key:      "amd.com/gpu.product-name",
														Operator: corev1.NodeSelectorOpIn,
														Values:   []string{"MI300"},
													},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			expected: "MI300",
		},
		{
			// A workload that constrains no accelerator is unresolved, whatever it
			// claims elsewhere. The acceleratorName label used to answer here and
			// was removed: nothing makes it true, since the scheduler is free to
			// place an unconstrained pod on any GPU node.
			name: "unconstrained_workload_is_unresolved",
			va: &llmdvariant.Variant{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"inference.optimization/acceleratorName": "H100",
					},
				},
			},
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{},
					},
				},
			},
			expected: constants.DefaultAcceleratorName,
		},
		{
			name: "nodeSelector_resolves_and_the_stale_label_is_ignored",
			va: &llmdvariant.Variant{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"inference.optimization/acceleratorName": "H100",
					},
				},
			},
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							NodeSelector: map[string]string{
								"nvidia.com/gpu.product": "A100",
							},
						},
					},
				},
			},
			expected: "A100",
		},
		{
			name: "nodeAffinity_resolves_and_the_stale_label_is_ignored",
			va: &llmdvariant.Variant{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"inference.optimization/acceleratorName": "H100",
					},
				},
			},
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Affinity: &corev1.Affinity{
								NodeAffinity: &corev1.NodeAffinity{
									RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
										NodeSelectorTerms: []corev1.NodeSelectorTerm{
											{
												MatchExpressions: []corev1.NodeSelectorRequirement{
													{
														Key:      "nvidia.com/gpu.product",
														Operator: corev1.NodeSelectorOpIn,
														Values:   []string{"V100"},
													},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			expected: "V100",
		},
		{
			// No scale target means no placement constraint to read, and a label
			// cannot stand in for one.
			name: "nil_deployment_is_unresolved",
			va: &llmdvariant.Variant{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"inference.optimization/acceleratorName": "T4",
					},
				},
			},
			deployment: nil,
			expected:   constants.DefaultAcceleratorName,
		},
		{
			name:       "nil_va_and_deployment_returns_default",
			va:         nil,
			deployment: nil,
			expected:   constants.DefaultAcceleratorName,
		},
		{
			name: "no_gpu_info_returns_default",
			va:   &llmdvariant.Variant{},
			deployment: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{},
					},
				},
			},
			expected: constants.DefaultAcceleratorName,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var scaleTarget scaletarget.ScaleTargetAccessor
			if tc.deployment != nil {
				scaleTarget = scaletarget.NewDeploymentAccessor(tc.deployment)
			}
			result := GetAcceleratorNameFromScaleTarget(tc.va, scaleTarget)
			assert.Equal(t, tc.expected, result)
		})
	}
}
