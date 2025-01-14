FROM cgr.dev/chainguard/static:latest
USER root

COPY gpu-metrics-agent /gpu-metrics-agent

ENTRYPOINT ["/gpu-metrics-agent"]
