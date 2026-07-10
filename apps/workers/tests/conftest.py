def pytest_configure(config):
    config.addinivalue_line(
        "filterwarnings", "ignore::starlette.exceptions.StarletteDeprecationWarning"
    )
