FROM python:3.14.10-slim@sha256:9aaa474097d094040649f2162a627c0049b408f52138d980916788d80f04802f
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*
RUN curl --fail --location --silent --show-error \
      https://s3.amazonaws.com/session-manager-downloads/plugin/1.2.896.0/ubuntu_arm64/session-manager-plugin.deb \
      -o /tmp/session-manager-plugin.deb \
    && echo '914eeca87b18b55356c58be197080f73742b3ca60d3823f0a45670dc9056aa9c  /tmp/session-manager-plugin.deb' | sha256sum -c - \
    && dpkg -i /tmp/session-manager-plugin.deb \
    && rm /tmp/session-manager-plugin.deb
RUN pip install --no-cache-dir ansible-core==2.21.5 boto3==1.43.108
COPY ansible/requirements.yml /tmp/requirements.yml
RUN ansible-galaxy collection install -r /tmp/requirements.yml
WORKDIR /workspace/ansible
ENTRYPOINT ["ansible-playbook"]
CMD ["site.yml"]
