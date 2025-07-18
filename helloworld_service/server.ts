Bun.serve({
  port: 8080,
  hostname: "0.0.0.0",
  fetch(req) {
    const url = new URL(req.url);
    
    if (req.method === "GET" && url.pathname === "/") {
      return new Response("Hello, World", {
        headers: { "Content-Type": "text/plain" },
      });
    }
    
    if (req.method === "GET" && url.pathname === "/health") {
      return new Response("OK", {
        headers: { "Content-Type": "text/plain" },
      });
    }
    
    return new Response("Not Found", { status: 404 });
  },
});

console.log("Server running on http://0.0.0.0:8080");