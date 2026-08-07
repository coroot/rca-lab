FROM php:8.3-fpm-alpine

RUN apk add --no-cache nginx libpq-dev $PHPIZE_DEPS \
    && docker-php-ext-install pdo_pgsql \
    && pecl install opentelemetry \
    && docker-php-ext-enable opentelemetry \
    && apk del $PHPIZE_DEPS \
    && rm -rf /var/cache/apk/*

COPY --from=composer:2 /usr/bin/composer /usr/bin/composer

COPY www.conf /usr/local/etc/php-fpm.d/www.conf
COPY nginx.conf /etc/nginx/nginx.conf
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

WORKDIR /app
COPY composer.json ./
RUN composer install --no-dev --no-interaction --no-progress

COPY public/ public/
COPY src/ src/
COPY variants/ variants/

ARG VARIANT=""
RUN if [ -n "$VARIANT" ]; then cp -rf variants/$VARIANT/src/. ./; fi

EXPOSE 8080

ENTRYPOINT ["/entrypoint.sh"]
