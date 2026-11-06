# Image the mock supplier runs for each "instance": Alpine with OpenSSH, root
# login by public key only. There is no GPU in it; it stands in for a rented
# machine so the provision -> SSH -> terminate lifecycle can be exercised.
FROM alpine:3.20
RUN apk add --no-cache openssh-server bash curl ca-certificates \
 && ssh-keygen -A \
 && mkdir -p /root/.ssh && chmod 700 /root/.ssh \
 && sed -i 's/^#\?PermitRootLogin.*/PermitRootLogin prohibit-password/' /etc/ssh/sshd_config \
 && sed -i 's/^#\?PasswordAuthentication.*/PasswordAuthentication no/' /etc/ssh/sshd_config \
 && passwd -u root 2>/dev/null || true
COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh
EXPOSE 22
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
