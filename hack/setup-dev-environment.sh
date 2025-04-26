#!/bin/bash
set -euo pipefail

# Configuration
REGISTRY_DOMAIN="registry.registry.svc.cluster.local"
REGISTRY_PORT="5000"
CLUSTER_NAME="rollout-dev"
CERTS_DIR="/tmp/kind-registry-certs"

# Ensure required tools are installed
command -v kind >/dev/null 2>&1 || { echo "kind is required but not installed. Aborting." >&2; exit 1; }
command -v docker >/dev/null 2>&1 || { echo "docker is required but not installed. Aborting." >&2; exit 1; }
command -v openssl >/dev/null 2>&1 || { echo "openssl is required but not installed. Aborting." >&2; exit 1; }

# Create directories
mkdir -p "${CERTS_DIR}"

# Generate self-signed certificates
echo "Generating self-signed certificates..."
openssl req -newkey rsa:4096 -nodes -sha256 \
  -keyout "${CERTS_DIR}/domain.key" \
  -x509 -days 365 -out "${CERTS_DIR}/domain.crt" \
  -subj "/CN=${REGISTRY_DOMAIN}" \
  -addext "subjectAltName = DNS:${REGISTRY_DOMAIN}"

# Create kind cluster configuration
cat <<EOF > /tmp/kind-config.yaml
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
  extraMounts:
  - hostPath: ${CERTS_DIR}/domain.crt
    containerPath: /usr/local/share/ca-certificates/registry-cert.crt
  extraPortMappings:
  - containerPort: 5000
    hostPort: 5001
    protocol: TCP
  - containerPort: 8080
    hostPort: 8080
    protocol: TCP
EOF

# Create kind cluster
echo "Creating Kind cluster..."
kind create cluster --name "${CLUSTER_NAME}" --config /tmp/kind-config.yaml

# Update certificates in the cluster nodes
echo "Updating certificates in the cluster..."
docker exec "${CLUSTER_NAME}-control-plane" update-ca-certificates

# Create registry namespace and TLS secret
echo "Creating registry namespace and TLS secret..."
kubectl create namespace registry || true
kubectl create secret tls registry-tls \
  --cert="${CERTS_DIR}/domain.crt" \
  --key="${CERTS_DIR}/domain.key" \
  -n registry || true

# Deploy the registry
echo "Deploying registry..."
cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Service
metadata:
  name: registry
  namespace: registry
spec:
  ports:
  - port: 443
    protocol: TCP
    targetPort: 5000
  selector:
    app: registry
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: registry
  namespace: registry
spec:
  selector:
    matchLabels:
      app: registry
  template:
    metadata:
      labels:
        app: registry
    spec:
      containers:
      - name: registry
        image: registry:2
        ports:
        - containerPort: 5000
          hostPort: 5000
        env:
        - name: REGISTRY_HTTP_TLS_CERTIFICATE
          value: /certs/tls.crt
        - name: REGISTRY_HTTP_TLS_KEY
          value: /certs/tls.key
        volumeMounts:
        - name: registry-tls
          mountPath: /certs
          readOnly: true
      volumes:
      - name: registry-tls
        secret:
          secretName: registry-tls
EOF

# Wait for registry to be ready
echo "Waiting for registry to be ready..."
kubectl wait --for=condition=available --timeout=60s deployment/registry -n registry

# Configure Docker to trust our self-signed certificate
echo "Configuring Docker to trust registry certificate..."
DOCKER_CERT_DIR="$HOME/.docker/certs.d/${REGISTRY_DOMAIN}:${REGISTRY_PORT}"
mkdir -p "${DOCKER_CERT_DIR}"
cp "${CERTS_DIR}/domain.crt" "${DOCKER_CERT_DIR}/ca.crt"

# Create registry certificate ConfigMap in controller namespace
echo "Creating registry certificate ConfigMap..."
kubectl create namespace rollout-controller-system || true
kubectl create configmap registry-certs \
  --from-file=ca.crt="${CERTS_DIR}/domain.crt" \
  -n rollout-controller-system || true

# Build the controller image
echo "Building controller image..."
make docker-build IMG="rollout-controller:dev-local"

# Load the image into kind
echo "Loading image into Kind cluster..."
kind load docker-image rollout-controller:dev-local --name "${CLUSTER_NAME}"

# Create certificate volume and init container configuration
echo "Creating certificate volume and init container configuration..."
cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: ConfigMap
metadata:
  name: cert-copy-script
  namespace: rollout-controller-system
data:
  copy-certs.sh: |
    #!/bin/sh
    mkdir -p /work-dir
    # Copy and follow symlinks from system certificates
    cp -rL /etc/ssl/certs/* /work-dir/
    # Add our registry certificate
    cp /registry-certs/ca.crt /work-dir/registry-cert.crt
---
apiVersion: v1
kind: Secret
metadata:
  name: registry-certs
  namespace: rollout-controller-system
data:
  ca.crt: $(base64 -i "${CERTS_DIR}/domain.crt")
---
EOF

# Deploy the controller
echo "Deploying controller..."
make deploy IMG="rollout-controller:dev-local"

# Patch the controller deployment to use init container for certificates
echo "Patching controller deployment to setup certificates..."
kubectl patch deployment -n rollout-controller-system rollout-controller-controller-manager --patch '
spec:
  template:
    spec:
      initContainers:
      - name: copy-certs
        image: golang:1.24
        command: ["/bin/sh", "/script/copy-certs.sh"]
        securityContext:
          runAsUser: 65532
          runAsGroup: 65532
          runAsNonRoot: true
        volumeMounts:
        - name: cert-script
          mountPath: /script
        - name: registry-certs
          mountPath: /registry-certs
        - name: ssl-certs
          mountPath: /work-dir
      containers:
      - name: manager
        volumeMounts:
        - name: ssl-certs
          mountPath: /etc/ssl/certs
          readOnly: true
      volumes:
      - name: cert-script
        configMap:
          name: cert-copy-script
          defaultMode: 0755
      - name: registry-certs
        secret:
          secretName: registry-certs
      - name: ssl-certs
        emptyDir: {}
'

echo "Setup completed successfully!"
echo "Your development environment is ready with:"
echo "- Kind cluster: ${CLUSTER_NAME}"
echo "- Registry: ${REGISTRY_DOMAIN}:${REGISTRY_PORT} (with TLS, running in-cluster)"
echo "- Controller deployed and running with registry certificates"
