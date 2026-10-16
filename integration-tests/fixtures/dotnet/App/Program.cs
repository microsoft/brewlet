using System.Net;
using System.Net.Sockets;
using System.Runtime.InteropServices;
using System.Text;
using System.Text.Json;

static class Identity
{
    [DllImport("libc", EntryPoint = "getuid")]
    public static extern uint User();

    [DllImport("libc", EntryPoint = "getgid")]
    public static extern uint Group();
}

static class Program
{
    static string Report(string[] args) => JsonSerializer.Serialize(new
    {
        language = "dotnet",
        runtime = Environment.Version.ToString(),
        dependency = Greeting.Message.Value,
        arguments = args,
        cwd = Directory.GetCurrentDirectory(),
        uid = Identity.User(),
        gid = Identity.Group(),
        path = Environment.GetEnvironmentVariable("PATH"),
        home = Environment.GetEnvironmentVariable("HOME"),
        value = Environment.GetEnvironmentVariable("BREWLET_TEST_VALUE")
    });

    static async Task Main(string[] args)
    {
        if (args.Contains("--probe"))
        {
            Console.WriteLine(Report(args));
            return;
        }
        int port = int.Parse(Environment.GetEnvironmentVariable("BREWLET_TEST_PORT") ?? "8000");
        using TcpListener listener = new(IPAddress.Any, port);
        listener.Start();
        Console.WriteLine(JsonSerializer.Serialize(new
        {
            listening = ((IPEndPoint)listener.LocalEndpoint).Port
        }));
        while (true)
        {
            using TcpClient client = await listener.AcceptTcpClientAsync();
            using NetworkStream stream = client.GetStream();
            using StreamReader reader = new(stream, leaveOpen: true);
            int headerLength = 0;
            string? line;
            while (!string.IsNullOrEmpty(line = await reader.ReadLineAsync()))
            {
                headerLength += line.Length;
                if (headerLength > 8192) throw new InvalidDataException("HTTP headers too large");
            }
            byte[] body = Encoding.UTF8.GetBytes(Report(args));
            byte[] headers = Encoding.ASCII.GetBytes(
                $"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {body.Length}\r\nConnection: close\r\n\r\n");
            await stream.WriteAsync(headers);
            await stream.WriteAsync(body);
        }
    }
}
