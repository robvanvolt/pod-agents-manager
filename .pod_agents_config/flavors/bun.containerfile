# --- Flavor: Bun ---
# Install via the official script which auto-detects OS (glibc vs musl) and arch.
# The COPY --from approach fails when the source image libc differs from the
# target (e.g. copying a glibc bun into Alpine/musl → "required file not found").
RUN curl -fsSL https://bun.sh/install | bash \
    && ln -sf /root/.bun/bin/bun /usr/local/bin/bun \
    && ln -sf /root/.bun/bin/bun /usr/local/bin/bunx