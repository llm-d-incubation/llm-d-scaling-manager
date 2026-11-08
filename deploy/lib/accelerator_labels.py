"""The node labels that name a GPU product, and the ones that state its memory.

NVIDIA GPU Feature Discovery writes `nvidia.com/gpu.product`, and every tool
here used to assume it. It is only the GFD spelling: managed providers label
their own way, so on those clusters the assumption does not fail loudly, it
reports an empty cluster. `warmpool.sh sizing` on CoreWeave said

    5 node(s) of: unknown  8x0 GiB GPU

about five 8-GPU H200 nodes, and then refused to recommend anything because it
could not read a capacity that is published under a different key.

This is the same list the controller resolves through -- `ProductLabelAliases`
in internal/constants/constants.go -- kept here because these tools run without
a Go toolchain. `hack/check-accelerator-labels.sh` fails when the two drift, so
a key added for the controller cannot silently skip the tooling.

Order matters: the canonical GFD key first, so a cluster that publishes both it
and a provider key resolves to the one the controller would use.
"""

# Keys whose VALUE is the GPU product name.
PRODUCT_KEYS = [
    "nvidia.com/gpu.product",              # NVIDIA GPU Feature Discovery
    "gpu.nvidia.com/model",                # CoreWeave CKS
    "gpu.nvidia.com/class",                # CoreWeave CKS: A100_NVLINK_80GB
    "cloud.google.com/gke-accelerator",    # GKE: nvidia-h100-80gb
    "eks.amazonaws.com/instance-gpu-name",  # EKS Auto Mode: h100
    "karpenter.k8s.aws/instance-gpu-name",  # Karpenter on AWS: h100
    "karpenter.azure.com/sku-gpu-name",    # AKS node auto-provisioning: A100
    "amd.com/gpu.product-name",            # AMD GPU Operator
    "beta.amd.com/gpu.product-name",       # standalone ROCm device-plugin labeller
    "habana.ai/product.name",              # UNVERIFIED -- see constants.go
    "gpu.intel.com/product",
]

# Keys whose VALUE is per-GPU memory, with what multiplies it into MiB.
#
# The unit is NOT uniform and cannot be guessed: GFD publishes MiB (81920 for an
# 80GB card) while CoreWeave publishes whole GiB (143). Treating one as the
# other is off by a factor of 1024 in the direction that silently declares a
# node too small, so a key is listed only where the unit is known. Anything
# absent from here reads as "not published", which the callers already handle.
MEMORY_KEYS_TO_MIB = [
    ("nvidia.com/gpu.memory", 1),      # GFD: MiB
    ("amd.com/gpu.memory", 1),         # AMD GPU Operator: MiB
    ("gpu.nvidia.com/vram", 1024),     # CoreWeave CKS: whole GiB
]


def product_of(labels, default="unknown"):
    """The GPU product these node labels name, or `default` if none do."""
    labels = labels or {}
    for key in PRODUCT_KEYS:
        value = labels.get(key)
        if value:
            return value
    return default


def product_key_of(labels):
    """Which key carried it -- for messages that must name a real key."""
    labels = labels or {}
    for key in PRODUCT_KEYS:
        if labels.get(key):
            return key
    return None


def per_gpu_mib(labels):
    """Per-GPU memory in MiB from these node labels, or None if unpublished."""
    labels = labels or {}
    for key, to_mib in MEMORY_KEYS_TO_MIB:
        value = labels.get(key)
        if value:
            try:
                return float(value) * to_mib
            except ValueError:
                # A label that is present but not a number is worse than an
                # absent one, because it looks like an answer. Skip it and let
                # the caller report the capacity as unreadable.
                continue
    return None
