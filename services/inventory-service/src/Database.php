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
            $user = $parts['user'] ?? '';
            $pass = $parts['pass'] ?? '';

            $dsn = "pgsql:host={$host};port={$port};dbname={$dbname};sslmode=require";

            self::$instance = new PDO($dsn, $user, $pass, [
                PDO::ATTR_ERRMODE => PDO::ERRMODE_EXCEPTION,
                PDO::ATTR_DEFAULT_FETCH_MODE => PDO::FETCH_ASSOC,
                PDO::ATTR_PERSISTENT => true,
            ]);
        }
        return self::$instance;
    }
}
