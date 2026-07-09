package handlers

import "net/http"

const swaggerUIHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Orpheus API &mdash; Swagger UI</title>
  <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5/swagger-ui.css">
  <link rel="icon" type="image/png" href="https://unpkg.com/swagger-ui-dist@5/favicon-32x32.png" sizes="32x32">
  <style>
    html, body { margin: 0; padding: 0; height: 100%; }
    body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif; }
  </style>
</head>
<body>
  <div id="swagger-ui"></div>
  <script src="https://unpkg.com/swagger-ui-dist@5/swagger-ui-bundle.js" crossorigin></script>
  <script>
    window.onload = function () {
      window.ui = SwaggerUIBundle({
        url: "/api/openapi.json",
        dom_id: "#swagger-ui",
        deepLinking: true,
        presets: [
          SwaggerUIBundle.presets.apis
        ]
      });
    };
  </script>
</body>
</html>`

const reDocHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Orpheus API &mdash; ReDoc</title>
  <style>
    html, body { margin: 0; padding: 0; height: 100%; }
  </style>
</head>
<body>
  <redoc spec-url="/api/openapi.json"></redoc>
  <script src="https://unpkg.com/redoc@2/bundles/redoc.standalone.js"></script>
</body>
</html>`

// SwaggerUI serves the Swagger UI HTML page. The page fetches the OpenAPI
// document from /api/openapi.json at runtime, so changes to the spec are
// reflected without restarting the server.
func SwaggerUI(w http.ResponseWriter, _ *http.Request) {
	writeHTML(w, swaggerUIHTML)
}

// ReDocUI serves the ReDoc HTML page. Like SwaggerUI, it fetches the OpenAPI
// document from /api/openapi.json at runtime.
func ReDocUI(w http.ResponseWriter, _ *http.Request) {
	writeHTML(w, reDocHTML)
}

func writeHTML(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body))
}
