<?php

require 'greeting.php';

function report(array $arguments): array
{
    return [
        'language' => 'php',
        'runtime' => PHP_VERSION,
        'dependency' => message(),
        'arguments' => $arguments,
        'cwd' => getcwd(),
        'uid' => posix_getuid(),
        'gid' => posix_getgid(),
        'path' => getenv('PATH') === false ? null : getenv('PATH'),
        'home' => getenv('HOME') === false ? null : getenv('HOME'),
        'value' => getenv('BREWLET_TEST_VALUE') === false ? null : getenv('BREWLET_TEST_VALUE'),
    ];
}

$arguments = array_slice($argv, 1);
if (in_array('--probe', $arguments, true)) {
    echo json_encode(report($arguments), JSON_UNESCAPED_SLASHES), "\n";
    exit(0);
}

$port = getenv('BREWLET_TEST_PORT');
$server = stream_socket_server('tcp://0.0.0.0:' . ($port === false ? '8000' : $port), $code, $error);
if ($server === false) {
    fwrite(STDERR, "listen failed: $error\n");
    exit(1);
}
$address = stream_socket_get_name($server, false);
echo json_encode(['listening' => (int) substr($address, strrpos($address, ':') + 1)]), "\n";
while (true) {
    $connection = @stream_socket_accept($server, -1);
    if ($connection === false) {
        continue;
    }
    stream_set_timeout($connection, 10);
    while (($line = fgets($connection)) !== false && rtrim($line, "\r\n") !== '') {
    }
    $body = json_encode(report($arguments), JSON_UNESCAPED_SLASHES);
    fwrite($connection, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: "
        . strlen($body) . "\r\nConnection: close\r\n\r\n" . $body);
    fclose($connection);
}
