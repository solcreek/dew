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
    && sed -i 's/=m$/=y/' .config \
    && make -j"$(nproc)" bzImage \
    && cp arch/x86/boot/bzImage /vmlinuz-turbo

FROM scratch
COPY --from=builder /vmlinuz-turbo /vmlinuz-turbo
