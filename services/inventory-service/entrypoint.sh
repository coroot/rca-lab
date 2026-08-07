#!/bin/sh
set -e

# Start php-fpm in background
php-fpm -D

# Trap signals and forward to both processes
trap 'kill -TERM $(cat /run/nginx.pid) ; kill -QUIT $(pgrep -f "php-fpm: master") ; wait' TERM QUIT

# Start nginx in foreground
exec nginx -g 'daemon off;'
