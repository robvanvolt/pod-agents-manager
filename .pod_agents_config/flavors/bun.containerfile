# --- Flavor: Bun ---
COPY --from=docker.io/oven/bun:latest /usr/local/bin/bun /usr/local/bin/bun
RUN ln -s /usr/local/bin/bun /usr/local/bin/bunx