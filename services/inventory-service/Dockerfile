FROM php:8.3-fpm-alpine

RUN apk add --no-cache nginx libpq-dev $PHPIZE_DEPS \
    && docker-php-ext-install pdo_pgsql \
    && pecl install opentelemetry \
    && docker-php-ext-enable opentelemetry \
    && apk del $PHPIZE_DEPS \
    && rm -rf /var/cache/apk/*

# Production OPcache: cache compiled bytecode and stop stat-ing files on every
# request (the code is baked into the image, so it never changes). Sized for the
# OpenTelemetry SDK's large class graph. This is a major per-request CPU saving
# for a PHP-FPM service.
RUN { \
      echo 'opcache.enable=1'; \
      echo 'opcache.validate_timestamps=0'; \
      echo 'opcache.memory_consumption=192'; \
      echo 'opcache.interned_strings_buffer=16'; \
      echo 'opcache.max_accelerated_files=20000'; \
    } > /usr/local/etc/php/conf.d/opcache.ini

COPY --from=composer:2 /usr/bin/composer /usr/bin/composer

COPY www.conf /usr/local/etc/php-fpm.d/www.conf
COPY nginx.conf /etc/nginx/nginx.conf
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

WORKDIR /app
COPY composer.json ./
# --optimize-autoloader builds a classmap so class loading doesn't scan the
# filesystem per request (costly with the OpenTelemetry SDK's many classes).
RUN composer install --no-dev --no-interaction --no-progress --optimize-autoloader

COPY public/ public/
COPY src/ src/
COPY variants/ variants/

ARG VARIANT=""
RUN if [ -n "$VARIANT" ]; then cp -rf variants/$VARIANT/src/. ./; fi

EXPOSE 8080

ENTRYPOINT ["/entrypoint.sh"]
