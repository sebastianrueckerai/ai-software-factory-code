docker build -q -f "$code/Dockerfile.runner" --target cosign -t $img:cosign "$code" >/dev/null
docker build -q -t $img - >/dev/null <<DOCKERFILE
# pin docker.io/alpine/k8s
FROM docker.io/alpine/k8s:1.36.1@sha256:51865f04b31c2f1d18db2c0964f4186d359007beedb9e478d342a7e504c0a3f3
