FROM debian:stable
USER root

COPY gpu-metrics-agent /gpu-metrics-agent

ENTRYPOINT ["/gpu-metrics-agent"]
