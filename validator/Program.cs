// verso-validate FILE.docx [--json]: Microsoft's Open XML SDK schema and
// semantic validation, as one JSON object. Exit 0 only when the file is valid.
using System.Security.Cryptography;
using System.Text.Json;
using DocumentFormat.OpenXml;
using DocumentFormat.OpenXml.Packaging;
using DocumentFormat.OpenXml.Validation;

if (args.Length < 1) { Console.Error.WriteLine("usage: verso-validate FILE.docx [--json]"); return 2; }
var path = args[0];
var sdk = typeof(OpenXmlElement).Assembly.GetName().Version!;
var result = new Dictionary<string, object?>
{
    ["Validator"] = $"microsoft-openxml:{sdk.Major}.{sdk.Minor}.{sdk.Build}",
    ["Sha256"] = Convert.ToHexString(SHA256.HashData(File.ReadAllBytes(path))).ToLowerInvariant(),
    ["Path"] = path,
    ["Ok"] = false,
    ["OpenError"] = null,
    ["RawErrorCount"] = 0,
    ["Errors"] = new List<object>(),
};
try
{
    using var doc = WordprocessingDocument.Open(path, false);
    var errors = new OpenXmlValidator(FileFormatVersions.Office2019).Validate(doc).Take(50).ToList();
    result["RawErrorCount"] = errors.Count;
    result["Errors"] = errors.Select(e => new { e.Description, e.Id, Part = e.Part?.Uri.ToString(), ErrorType = e.ErrorType.ToString(), e.Path?.XPath }).ToList();
    result["Ok"] = errors.Count == 0;
}
catch (Exception e)
{
    result["OpenError"] = e.Message;
}
Console.WriteLine(JsonSerializer.Serialize(result));
return (bool)result["Ok"]! ? 0 : 1;
