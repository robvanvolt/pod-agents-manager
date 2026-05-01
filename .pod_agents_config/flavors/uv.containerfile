# --- Flavor: UV ---
COPY --from=ghcr.io/astral-sh/uv:latest /uv /uvx /bin/

# 1. Define the virtual environment path and add it to the system PATH
ENV VIRTUAL_ENV=/opt/venv
ENV PATH="$VIRTUAL_ENV/bin:$PATH"

# 2. Tell uv to create a venv. 
# Explicitly specifying the Python version forces uv to download Python 
# if it isn't already installed in the node-slim base image.
RUN uv venv --python 3.12 $VIRTUAL_ENV

# 3. Preinstall your data science packages (or anything else)
RUN uv pip install pandas pyarrow pillow

# 4. Inject the visual (venv) prompt for interactive human sessions
RUN echo "source /opt/venv/bin/activate" >> /root/.bashrc && \
    echo "source /opt/venv/bin/activate" >> /etc/bash.bashrc

# 5. THE BULLETPROOF FIX: Symlink the venv binaries globally
# This ensures non-interactive AI background shells hit the venv immediately, 
# even if they wipe the $PATH or explicitly type `python3` instead of `python`.
RUN ln -sf /opt/venv/bin/python /usr/local/bin/python && \
    ln -sf /opt/venv/bin/python /usr/local/bin/python3 && \
    ln -sf /opt/venv/bin/pip /usr/local/bin/pip && \
    ln -sf /opt/venv/bin/pip /usr/local/bin/pip3
