# Minimal build-context image: proves `nerdctl compose` can build from source.
# Requires BuildKit (buildkitd) inside the guest, which the standard profile
# does not currently package — this is the risk #1 probe.
FROM alpine:3.20
RUN apk add --no-cache busybox-extras
COPY index.html /www/index.html
EXPOSE 80
CMD ["httpd", "-f", "-p", "80", "-h", "/www"]
