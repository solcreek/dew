FROM alpine:3.21 AS builder

ARG KERNEL_VERSION=6.12.91
ARG KERNEL_SERIES=6.12

RUN apk add --no-cache \
    build-base bc bison flex elfutils-dev openssl openssl-dev perl \
    linux-headers diffutils findutils gmp-dev mpc1-dev mpfr-dev

RUN wget -q "https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-${KERNEL_VERSION}.tar.xz" \
    && tar xf "linux-${KERNEL_VERSION}.tar.xz" \
    && rm "linux-${KERNEL_VERSION}.tar.xz"

WORKDIR /linux-${KERNEL_VERSION}

COPY config-turbo .config

RUN make olddefconfig \
    && sed -i 's/CONFIG_MODULE_SIG=y/CONFIG_MODULE_SIG=n/' .config \
    && sed -i 's/CONFIG_SYSTEM_TRUSTED_KEYRING=y/# CONFIG_SYSTEM_TRUSTED_KEYRING is not set/' .config \
    && sed -i 's/CONFIG_EXT4_FS=m/CONFIG_EXT4_FS=y/' .config \
    && sed -i 's/CONFIG_OVERLAY_FS=m/CONFIG_OVERLAY_FS=y/' .config \
    && sed -i 's/CONFIG_BRIDGE=m/CONFIG_BRIDGE=y/' .config \
    && sed -i 's/CONFIG_VETH=m/CONFIG_VETH=y/' .config \
    && sed -i 's/CONFIG_NF_CONNTRACK=m/CONFIG_NF_CONNTRACK=y/' .config \
    && sed -i 's/CONFIG_NF_NAT=m/CONFIG_NF_NAT=y/' .config \
    && sed -i 's/CONFIG_IP_NF_IPTABLES=m/CONFIG_IP_NF_IPTABLES=y/' .config \
    && sed -i 's/CONFIG_IP_NF_FILTER=m/CONFIG_IP_NF_FILTER=y/' .config \
    && sed -i 's/CONFIG_IP_NF_NAT=m/CONFIG_IP_NF_NAT=y/' .config \
    && sed -i 's/CONFIG_PACKET=m/CONFIG_PACKET=y/' .config \
    && make -j"$(nproc)" bzImage \
    && cp arch/x86/boot/bzImage /vmlinuz-turbo

FROM scratch
COPY --from=builder /vmlinuz-turbo /vmlinuz-turbo
