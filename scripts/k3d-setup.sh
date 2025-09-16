#!/bin/bash

# K3d setup script for local Kubernetes testing
# Usage: ./scripts/k3d-setup.sh [staging|test]

set -e

ENVIRONMENT=${1:-staging}
CLUSTER_NAME="forge-${ENVIRONMENT}"
K3D_VERSION="v5.6.0"

echo "Setting up k3d cluster for ${ENVIRONMENT} environment..."

# Check if k3d is installed
if ! command -v k3d &> /dev/null; then
    echo "k3d not found. Installing..."
    curl -s https://raw.githubusercontent.com/k3d-io/k3d/main/install.sh | TAG=${K3D_VERSION} bash
fi

# Delete existing cluster if it exists
if k3d cluster list | grep -q ${CLUSTER_NAME}; then
    echo "Deleting existing cluster ${CLUSTER_NAME}..."
    k3d cluster delete ${CLUSTER_NAME}
fi

# Create new cluster with appropriate resources
echo "Creating k3d cluster ${CLUSTER_NAME}..."
k3d cluster create ${CLUSTER_NAME} \
    --servers 1 \
    --agents 2 \
    --port "8080:80@loadbalancer" \
    --port "8443:443@loadbalancer" \
    --port "50051:50051@loadbalancer" \
    --volume "$(pwd)/configs/${ENVIRONMENT}:/configs@all" \
    --k3s-arg "--disable=traefik@server:0" \
    --registry-create ${CLUSTER_NAME}-registry:0.0.0.0:5000

echo "Waiting for cluster to be ready..."
kubectl wait --for=condition=Ready nodes --all --timeout=60s

# Create namespace
kubectl create namespace ${ENVIRONMENT} || true

# Install metrics server for HPA
kubectl apply -f https://github.com/kubernetes-sigs/metrics-server/releases/latest/download/components.yaml

echo "Applying configurations for ${ENVIRONMENT}..."
kubectl apply -k deployments/k8s/overlays/${ENVIRONMENT}/ -n ${ENVIRONMENT}

echo "Cluster ${CLUSTER_NAME} is ready!"
echo ""
echo "To use this cluster:"
echo "  export KUBECONFIG=$(k3d kubeconfig get ${CLUSTER_NAME})"
echo ""
echo "To deploy your application:"
echo "  kubectl apply -k deployments/k8s/overlays/${ENVIRONMENT}/ -n ${ENVIRONMENT}"
echo ""
echo "To access services:"
echo "  - HTTP: http://localhost:8080"
echo "  - HTTPS: https://localhost:8443"
echo "  - gRPC: localhost:50051"
echo ""
echo "To delete the cluster:"
echo "  k3d cluster delete ${CLUSTER_NAME}"