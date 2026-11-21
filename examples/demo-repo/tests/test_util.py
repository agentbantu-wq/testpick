from app.util import slug


def test_slug():
    assert slug("Hello World") == "hello-world"
