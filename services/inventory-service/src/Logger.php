<?php

use Monolog\Handler\StreamHandler;
use Monolog\Logger as MonologLogger;
use OpenTelemetry\API\Globals;
use OpenTelemetry\Contrib\Logs\Monolog\Handler as OtelHandler;
use Psr\Log\LogLevel;

class Logger
{
    private static ?MonologLogger $instance = null;

    public static function get(): MonologLogger
    {
        if (self::$instance === null) {
            self::$instance = new MonologLogger('inventory-service');
            self::$instance->pushHandler(new StreamHandler('php://stdout', LogLevel::DEBUG));
            self::$instance->pushHandler(new OtelHandler(Globals::loggerProvider(), LogLevel::INFO));
        }
        return self::$instance;
    }
}
