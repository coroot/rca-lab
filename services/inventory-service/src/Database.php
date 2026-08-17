<?php

class Database
{
    private static ?PDO $instance = null;

    public static function get(): PDO
    {
        if (self::$instance === null) {
            $url = getenv('DATABASE_URL');
            if (!$url) {
                throw new RuntimeException('DATABASE_URL not set');
            }

            $parts = parse_url($url);
            $host = $parts['host'] ?? 'localhost';
            $port = $parts['port'] ?? 5432;
            $dbname = ltrim($parts['path'] ?? '/inventory', '/');
            // parse_url() leaves userinfo percent-encoded; the operator's
            // pgbouncer-uri encodes special characters in the password, so
            // decode before handing them to PDO (otherwise SASL auth fails).
            $user = urldecode($parts['user'] ?? '');
            $pass = urldecode($parts['pass'] ?? '');

            $dsn = "pgsql:host={$host};port={$port};dbname={$dbname};sslmode=require";

            // Non-persistent: a persistent connection left mid-transaction (a
            // request killed at the FPM timeout) would linger in the pool
            // idle-in-transaction, holding locks. A fresh per-request connection
            // is cleaned up at request end instead.
            self::$instance = new PDO($dsn, $user, $pass, [
                PDO::ATTR_ERRMODE => PDO::ERRMODE_EXCEPTION,
                PDO::ATTR_DEFAULT_FETCH_MODE => PDO::FETCH_ASSOC,
                PDO::ATTR_PERSISTENT => false,
            ]);
            // Fail fast rather than hang past the FPM request timeout (which
            // would kill the worker mid-query and orphan the transaction).
            self::$instance->exec("SET statement_timeout = '10s'");
            self::$instance->exec("SET lock_timeout = '5s'");
        }
        return self::$instance;
    }
}
