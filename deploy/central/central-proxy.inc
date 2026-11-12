# Shared by the two locations in nginx.example.conf.
proxy_http_version 1.1;
proxy_set_header Host $host;
proxy_set_header Authorization $http_authorization;
proxy_read_timeout 30s;
