// Decodes a PNG with jsQR and prints the decoded text.
//
// It is driven by qr_test.go, which needs an INDEPENDENT decoder: a QR encoder
// that is only checked against itself can be wrong in exactly the way that
// matters (a symbol that looks right and does not scan).
//
// Usage: node testdata/decode.js <png-path>
// QR_NODE_MODULES overrides where jsqr/pngjs are installed.
const modules = process.env.QR_NODE_MODULES || '/tmp/qrverify/node_modules';
const fs = require('fs');
const jsQR = require(modules + '/jsqr');
const { PNG } = require(modules + '/pngjs');

const file = process.argv[2];
if (!file) {
	console.error('usage: decode.js <png-path>');
	process.exit(2);
}
const image = PNG.sync.read(fs.readFileSync(file));
const result = jsQR(new Uint8ClampedArray(image.data), image.width, image.height);
if (!result) {
	console.error('jsQR decoded nothing in ' + file + ' (' + image.width + 'x' + image.height + ')');
	process.exit(1);
}
console.log(result.data);
