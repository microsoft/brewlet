using System.Net;
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
        using HttpListener listener = new();
        listener.Prefixes.Add("http://+:8000/");
        listener.Start();
        while (true)
        {
            HttpListenerContext context = await listener.GetContextAsync();
            byte[] body = Encoding.UTF8.GetBytes(Report(args));
            context.Response.ContentType = "application/json";
            context.Response.ContentLength64 = body.Length;
            await context.Response.OutputStream.WriteAsync(body);
            context.Response.Close();
        }
    }
}
